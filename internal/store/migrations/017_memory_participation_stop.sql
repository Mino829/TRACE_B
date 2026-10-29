-- Participation stop is separate from expiry and whole-session deletion.
ALTER TABLE memory_sessions ADD COLUMN stopped_at TIMESTAMPTZ;

-- Photo mutation receipts disappear with the owning Memory session. Whole-session
-- delete receipts deliberately remain unbound for their existing 24-hour replay.
ALTER TABLE idempotency_records ADD COLUMN memory_session_id TEXT
 REFERENCES memory_sessions(id) ON DELETE CASCADE;
CREATE INDEX ON idempotency_records(memory_session_id) WHERE memory_session_id IS NOT NULL;

-- Recover ownership of existing identifiable photo receipts without discarding
-- legacy records whose one-way scope no longer has a surviving photo locator.
-- Those unidentifiable old records retain their already assigned expiry.
WITH photo_scopes AS (
 SELECT id AS memory_session_id,'create:photo:' || id AS scope FROM memory_sessions
 UNION ALL
 SELECT id,'create:moment:' || id FROM memory_sessions
 UNION ALL
 SELECT memory_session_id,'delete:photo:' || memory_session_id || ':' || id FROM photos
 UNION ALL
 SELECT memory_session_id,'delete:moment:' || memory_session_id || ':' || id FROM photo_moments
)
UPDATE idempotency_records r SET memory_session_id=m.id,expires_at=LEAST(r.expires_at,m.delete_after)
FROM photo_scopes p JOIN memory_sessions m ON m.id=p.memory_session_id
WHERE r.scope=encode(sha256(convert_to(p.scope,'UTF8')),'hex');
