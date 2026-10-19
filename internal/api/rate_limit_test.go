package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mino829/trace_Backend/internal/config"
)

func TestPositionRateAllowsFiveSecondTrackingAndRecovers(t *testing.T) {
	limiter := newRateLimiter()
	start := time.Unix(1_700_000_000, 0)
	rule := rateRule{key: "position/live-session", rate: 0.2, burst: 5}
	for i := range 5 {
		if got := limiter.allow(start, rule); got != 0 {
			t.Fatalf("burst request %d: retry %d", i, got)
		}
	}
	if got := limiter.allow(start, rule); got != 5 {
		t.Fatalf("sixth request must wait five seconds, got %d", got)
	}
	for i := 1; i <= 12; i++ {
		if got := limiter.allow(start.Add(time.Duration(i)*5*time.Second), rule); got != 0 {
			t.Fatalf("normal five-second update %d: retry %d", i, got)
		}
	}
	if got := limiter.allow(start.Add(60*time.Second), rule); got != 5 {
		t.Fatalf("extra update after 12/min: retry %d", got)
	}
	if got := limiter.allow(start.Add(65*time.Second), rule); got != 0 {
		t.Fatalf("quota did not recover: %d", got)
	}
}

func TestCombinedLimitsDoNotDrainIPOnGlobalRejection(t *testing.T) {
	l := newRateLimiter()
	now := time.Unix(1_700_000_000, 0)
	global := rateRule{key: "create/global", rate: 10, burst: 10}
	ip := rateRule{key: "create/ip/shared", rate: 1, burst: 60}
	for range 10 {
		if got := l.allow(now, ip, global); got != 0 {
			t.Fatal(got)
		}
	}
	if got := l.allow(now, ip, global); got != 1 {
		t.Fatalf("global limit: retry %d", got)
	}
	// A blocked attempt didn't use an IP token. One second later both
	// quotas recover enough to accept another request.
	if got := l.allow(now.Add(time.Second), ip, global); got != 0 {
		t.Fatalf("global quota did not recover: %d", got)
	}
	l.mu.Lock()
	remaining := l.buckets[ip.key].tokens
	l.mu.Unlock()
	if remaining != 50 {
		t.Fatalf("rejected request consumed IP quota: %.0f", remaining)
	}
}

func TestRateBucketCapacityFailsClosedAndRecovers(t *testing.T) {
	l := newRateLimiter()
	start := time.Unix(1_700_000_000, 0)
	for i := range maxRateBuckets {
		if got := l.allow(start, rateRule{key: fmt.Sprintf("credential/%d", i), rate: 1, burst: 1}); got != 0 {
			t.Fatalf("premature capacity limit at %d: %d", i, got)
		}
	}
	rule := rateRule{key: "new-participant", rate: 1, burst: 1}
	if got := l.allow(start, rule); got != 60 || len(l.buckets) != maxRateBuckets {
		t.Fatalf("over-capacity request: retry=%d buckets=%d", got, len(l.buckets))
	}
	if got := l.allow(start.Add(2*time.Minute), rule); got != 0 {
		t.Fatalf("inactive buckets were not evicted: %d", got)
	}
}

func TestBearerFormattingDoesNotResetSessionLimit(t *testing.T) {
	s := &Server{cfg: config.Default(), now: func() time.Time { return time.Unix(1_700_000_000, 0) }, limiter: newRateLimiter()}
	h := s.limitedSession("position", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, authorization := range []string{
		"Bearer participant-token", "bearer participant-token", "BEARER participant-token",
		"Bearer  participant-token", "Bearer participant-token  ",
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/live-presence-sessions/live_abc/position", nil)
		req.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		h(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("normal token formatting rejected: %q status %d", authorization, response.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/live-presence-sessions/live_abc/position", nil)
	req.Header.Set("Authorization", "bEaReR participant-token")
	response := httptest.NewRecorder()
	h(response, req)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("same token bypassed position quota: %d retry=%s", response.Code, response.Header().Get("Retry-After"))
	}
}

func TestConcurrentSessionLimit(t *testing.T) {
	l := newRateLimiter()
	now := time.Unix(1_700_000_000, 0)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.allow(now, rateRule{key: "position/a", rate: 0.2, burst: 5}) == 0 {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := accepted.Load(); got != 5 {
		t.Fatalf("concurrent accepted requests: %d", got)
	}
	if got := l.allow(now, rateRule{key: "position/b", rate: 0.2, burst: 5}); got != 0 {
		t.Fatalf("other session was throttled: %d", got)
	}
}

func TestSharedNetworkSupportsFiftyFiveSecondSessions(t *testing.T) {
	l := newRateLimiter()
	start := time.Unix(1_700_000_000, 0)
	for tick := range 12 {
		at := start.Add(time.Duration(tick) * 5 * time.Second)
		for participant := range 50 {
			if got := l.allow(at,
				rateRule{key: fmt.Sprintf("position/%d", participant), rate: 0.2, burst: 5},
				rateRule{key: "authenticated/ip/shared", rate: 20, burst: 120},
				rateRule{key: "api/global", rate: 100, burst: 100}); got != 0 {
				t.Fatalf("participant %d at five-second tick %d: retry %d", participant, tick, got)
			}
		}
	}
}

func TestPublicIPLimitRejectsAndRecoversWithoutBlockingAnotherIP(t *testing.T) {
	cfg := config.Default()
	cfg.EdgeSharedSecret = "a-32-byte-secret-for-cloudflare-edge"
	now := time.Unix(1_700_000_000, 0)
	s := &Server{cfg: cfg, now: func() time.Time { return now }, limiter: newRateLimiter()}
	h := s.limitedPublic("create", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	send := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/experiences", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("X-Trace-Client-IP", ip)
		response := httptest.NewRecorder()
		h(response, req)
		return response
	}
	for range 6 {
		for range 10 {
			if r := send("203.0.113.1"); r.Code != http.StatusCreated {
				t.Fatalf("legitimate creation blocked: %d", r.Code)
			}
		}
		now = now.Add(time.Second)
	}
	for range 6 {
		if r := send("203.0.113.1"); r.Code != http.StatusCreated {
			t.Fatalf("replenished IP quota blocked: %d", r.Code)
		}
	}
	if r := send("203.0.113.1"); r.Code != http.StatusTooManyRequests || r.Header().Get("Retry-After") != "1" {
		t.Fatalf("IP limit: status=%d retry=%q", r.Code, r.Header().Get("Retry-After"))
	}
	if r := send("203.0.113.2"); r.Code != http.StatusCreated {
		t.Fatalf("another IP blocked by first IP: %d", r.Code)
	}
	now = now.Add(time.Second)
	if r := send("203.0.113.1"); r.Code != http.StatusCreated {
		t.Fatalf("IP quota did not recover: %d", r.Code)
	}
}

func TestCreationIPTrustBoundary(t *testing.T) {
	cfg := config.Default()
	cfg.EdgeSharedSecret = "a-32-byte-secret-for-cloudflare-edge"
	s := &Server{cfg: cfg, now: time.Now, limiter: newRateLimiter()}
	for _, tt := range []struct {
		name, remote, forwarded, want string
	}{
		{"worker ipv4", "10.0.0.1:1234", "203.0.113.41", "203.0.113.41"},
		{"worker ipv6", "10.0.0.1:1234", "2001:db8::1", "2001:db8::1"},
		{"invalid forwarding", "10.0.0.1:1234", "203.0.113.41, 198.51.100.1", "10.0.0.1"},
		{"no forwarding", "10.0.0.1:1234", "", "10.0.0.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/experiences", nil)
			r.RemoteAddr = tt.remote
			r.Header.Set("X-Trace-Client-IP", tt.forwarded)
			r.Header.Set("X-Forwarded-For", "198.51.100.7")
			if got := s.clientIP(r); got != tt.want {
				t.Fatalf("client IP: %q != %q", got, tt.want)
			}
		})
	}
	s.cfg.EdgeSharedSecret = ""
	r := httptest.NewRequest(http.MethodPost, "/v1/experiences", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Trace-Client-IP", "203.0.113.41")
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	if got := s.clientIP(r); got != "192.0.2.1" {
		t.Fatalf("direct request trusted forwarding header: %q", got)
	}
}
