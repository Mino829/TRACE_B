-- Stop prior revisions and drain the queue before upgrading through migration 012.
-- Previously skipped arrivals remain terminal; never automatically replay them.
ALTER TABLE arrival_events DROP COLUMN replay_deadline;
ALTER TABLE arrival_events ADD CONSTRAINT arrival_photo_wait_after_arrival
 CHECK (photo_wait_until >= arrived_at);

-- Existing conflicting playback is an operator-visible migration failure,
-- not a reason to silently skip or resurrect an arrival.
CREATE UNIQUE INDEX arrival_events_single_playing ON arrival_events ((TRUE))
 WHERE replay_status='playing';
