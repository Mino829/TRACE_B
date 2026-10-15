-- Display positions are now volatile per-process state. No existing positions are copied.
DROP TABLE live_positions;

-- Expiring the link credential must not erase the relationship needed until
-- the earlier session deletion deadline. Backfill from the old link deadline.
ALTER TABLE experience_links ADD COLUMN token_expires_at TIMESTAMPTZ;
UPDATE experience_links el SET token_expires_at=el.expires_at,
 expires_at=LEAST(m.delete_after,l.delete_after)
 FROM memory_sessions m,live_presence_sessions l
 WHERE el.memory_session_id=m.id AND el.live_session_id=l.id;
ALTER TABLE experience_links ALTER COLUMN token_expires_at SET NOT NULL;
