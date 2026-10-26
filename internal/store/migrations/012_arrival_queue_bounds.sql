-- Keep only the first ten unexpired arrivals. Commit the matching display
-- events with the state change so connected renderers can discard old requests.
WITH bounds AS (
 SELECT id, LEAST(replay_deadline, arrived_at + INTERVAL '30 seconds') AS deadline
 FROM arrival_events WHERE replay_status='queued'
), ranked AS (
 SELECT id, deadline,
  COUNT(*) FILTER (WHERE deadline > CURRENT_TIMESTAMP) OVER (ORDER BY a.arrived_at, a.id) AS live_position
 FROM bounds b JOIN arrival_events a USING (id)
), updated AS (
 UPDATE arrival_events a SET
  replay_deadline=r.deadline,
  photo_wait_until=LEAST(a.photo_wait_until,r.deadline),
  replay_status=CASE WHEN r.deadline<=CURRENT_TIMESTAMP OR r.live_position>10 THEN 'skipped' ELSE 'queued' END,
  skip_reason=CASE WHEN r.deadline<=CURRENT_TIMESTAMP THEN 'timeout' WHEN r.live_position>10 THEN 'queue_full' ELSE NULL END,
  replay_finished_at=CASE WHEN r.deadline<=CURRENT_TIMESTAMP OR r.live_position>10 THEN CURRENT_TIMESTAMP ELSE a.replay_finished_at END
 FROM ranked r WHERE a.id=r.id RETURNING a.id,a.skip_reason
), skipped AS (
 SELECT id,skip_reason FROM updated WHERE skip_reason IS NOT NULL
), cursor AS (
 UPDATE display_cursor SET last_id=last_id+(SELECT COUNT(*) FROM skipped)
 WHERE singleton=TRUE RETURNING last_id
)
INSERT INTO display_events(id,kind,data,occurred_at)
SELECT cursor.last_id-(SELECT COUNT(*) FROM skipped)+ROW_NUMBER() OVER (ORDER BY skipped.id),
 'arrival.replay.skipped',jsonb_build_object('arrivalId',skipped.id,'reason',skipped.skip_reason),CURRENT_TIMESTAMP
FROM skipped CROSS JOIN cursor;
