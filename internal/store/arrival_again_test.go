package store_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/testdb"
)

func arrivalReply(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var reply struct {
		ArrivalID string `json:"arrivalId"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatal(err)
	}
	return reply.ArrivalID, reply.Status
}

func displayEventCount(t *testing.T, f *testdb.Fixture, kind, arrivalID string) int {
	t.Helper()
	var count int
	if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM display_events WHERE kind=$1 AND data->>'arrivalId'=$2`,
		kind, arrivalID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestFinishedReplayPlaysAgainFromFreshQRWithLaterPhotos(t *testing.T) {
	for _, finished := range []string{"completed", "skipped"} {
		t.Run(finished, func(t *testing.T) {
			f := testdb.Open(t)
			checkpointFixture(t, f)
			f.Store.EnableDisplay()
			now := time.Now().UTC().Truncate(time.Microsecond)
			id := deviceExperience(t, f, now).MemorySessionID
			if _, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("before", id, "cp-a", now), now); err != nil {
				t.Fatal(err)
			}
			first := now.Add(time.Minute)
			if _, err := f.Store.AcceptArrival(t.Context(), id, "", "arrival-first", "mid", "key-first", "request-first", first); err != nil {
				t.Fatal(err)
			}
			switch finished {
			case "completed":
				if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease-first", first); err != nil || !ok {
					t.Fatalf("lease: %t %v", ok, err)
				}
				if status, err := f.Store.DisplayReplay(t.Context(), "holder", "lease-first", "start-first", "arrival-first", "arrival.replay.started", first); err != nil || status != "playing" {
					t.Fatalf("first start: %s %v", status, err)
				}
				if status, err := f.Store.DisplayReplay(t.Context(), "holder", "lease-first", "complete-first", "arrival-first", "arrival.replay.completed", first.Add(15*time.Second)); err != nil || status != "completed" {
					t.Fatalf("first completion: %s %v", status, err)
				}
			case "skipped":
				if err := f.Store.SkipOwnArrival(t.Context(), id, first.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}

			// A photo taken after the arrival stays off that arrival's route.
			taken := now.Add(2 * time.Minute)
			if _, _, err := f.Store.CreatePhotoMoment(t.Context(), momentInput("after", id, "cp-b", taken), taken); err != nil {
				t.Fatal(err)
			}
			segments, err := f.Store.MomentRouteSegments(t.Context(), id)
			if err != nil || len(segments) != 1 || segments[0].ToPhotoMomentID != nil || fmt.Sprint(segments[0].PathNodeIDs) != "[cp-a mid]" {
				t.Fatalf("later photo changed the finished route: %+v %v", segments, err)
			}

			// The earlier key, or a read without a fresh code, still only tells the state.
			again := now.Add(3 * time.Minute)
			for _, retry := range []struct{ checkpoint, key, request string }{
				{"mid", "key-first", "request-first"},
				{"", "key-expired-code", "request-expired-code"},
			} {
				body, err := f.Store.AcceptArrival(t.Context(), id, "", "arrival-not-again", retry.checkpoint, retry.key, retry.request, again)
				if err != nil {
					t.Fatalf("retry %s: %v", retry.key, err)
				}
				if arrivalID, status := arrivalReply(t, body); arrivalID != "arrival-first" || status != finished {
					t.Fatalf("retry %s queued again: %s", retry.key, body)
				}
			}
			if displayEventCount(t, f, "arrival.replay.requested", "arrival-not-again") != 0 {
				t.Fatal("a retry asked the renderer for another replay")
			}

			// A fresh code under a new key queues the same participation again, under a new ID.
			body, err := f.Store.AcceptArrival(t.Context(), id, "", "arrival-again", "mid", "key-again", "request-again", again)
			if err != nil {
				t.Fatal(err)
			}
			if arrivalID, status := arrivalReply(t, body); arrivalID != "arrival-again" || status != "queued" {
				t.Fatalf("fresh code did not queue again: %s", body)
			}
			var rows int
			var status, reason string
			var arrivedAt time.Time
			if err := f.SQL.QueryRowContext(t.Context(), `SELECT COUNT(*) OVER (),replay_status,COALESCE(skip_reason,''),arrived_at
 FROM arrival_events WHERE memory_session_id=$1`, id).Scan(&rows, &status, &reason, &arrivedAt); err != nil {
				t.Fatal(err)
			}
			if rows != 1 || status != "queued" || reason != "" || !arrivedAt.Equal(again) {
				t.Fatalf("arrival row did not start over: %d %s/%s %s", rows, status, reason, arrivedAt)
			}
			if displayEventCount(t, f, "participant.arrived", "arrival-again") != 1 || displayEventCount(t, f, "arrival.replay.requested", "arrival-again") != 1 {
				t.Fatal("renderer was not told of the new arrival")
			}
			segments, err = f.Store.MomentRouteSegments(t.Context(), id)
			if err != nil || len(segments) != 2 || fmt.Sprint(segments[0].PathNodeIDs) != "[cp-a mid cp-b]" ||
				segments[1].ToPhotoMomentID != nil || fmt.Sprint(segments[1].PathNodeIDs) != "[cp-b mid]" {
				t.Fatalf("later photo did not join the route: %+v %v", segments, err)
			}
			snapshot, err := f.Store.DisplaySnapshot(t.Context(), again)
			if err != nil || len(snapshot.Arrivals) != 1 || snapshot.Arrivals[0].ArrivalID != "arrival-again" ||
				len(snapshot.Arrivals[0].PhotoCheckpoints) != 2 || fmt.Sprint(snapshot.Arrivals[0].PathNodeIDs) != "[cp-a mid cp-b mid]" {
				t.Fatalf("display does not draw the later photo: %+v %v", snapshot, err)
			}
			candidates, err := f.Store.ReplayCandidates(t.Context(), again)
			if err != nil || len(candidates) != 1 || candidates[0].ArrivalID != "arrival-again" || candidates[0].ReadyPhotoCount != 2 {
				t.Fatalf("replay candidates: %+v %v", candidates, err)
			}

			// While it waits, another fresh code keeps its place instead of queueing twice.
			body, err = f.Store.AcceptArrival(t.Context(), id, "", "arrival-third", "mid", "key-third", "request-third", again.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if arrivalID, status := arrivalReply(t, body); arrivalID != "arrival-again" || status != "queued" {
				t.Fatalf("queued arrival queued twice: %s", body)
			}

			// The second replay plays and completes like the first.
			if ok, err := f.Store.AcquireDisplayLease(t.Context(), "holder", "lease-again", again); err != nil || !ok {
				t.Fatalf("lease again: %t %v", ok, err)
			}
			if status, err := f.Store.DisplayReplay(t.Context(), "holder", "lease-again", "start-again", "arrival-again", "arrival.replay.started", again); err != nil || status != "playing" {
				t.Fatalf("second start: %s %v", status, err)
			}
			if status, err := f.Store.DisplayReplay(t.Context(), "holder", "lease-again", "complete-again", "arrival-again", "arrival.replay.completed", again.Add(15*time.Second)); err != nil || status != "completed" {
				t.Fatalf("second completion: %s %v", status, err)
			}
		})
	}
}

func TestStoppedMemoryDoesNotPlayAgain(t *testing.T) {
	f := testdb.Open(t)
	f.Store.EnableDisplay()
	now := time.Now().UTC().Truncate(time.Microsecond)
	v := fixtureExperience(t, f, now)
	if _, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, v.LinkTokenHash, "arrival-stopped", "booth", "key-first", "request-first", now); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.SkipOwnArrival(t.Context(), v.MemorySessionID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.StopMemory(t.Context(), v.MemorySessionID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	body, err := f.Store.AcceptArrival(t.Context(), v.MemorySessionID, v.LinkTokenHash, "arrival-after-stop", "booth", "key-after-stop", "request-after-stop", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if arrivalID, status := arrivalReply(t, body); arrivalID != "arrival-stopped" || status != "skipped" {
		t.Fatalf("stopped participation queued again: %s", body)
	}
	if displayEventCount(t, f, "arrival.replay.requested", "arrival-after-stop") != 0 {
		t.Fatal("stopped participation asked the renderer for a replay")
	}
}
