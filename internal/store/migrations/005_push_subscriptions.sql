ALTER TABLE memory_sessions ADD COLUMN push_stopped BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE push_subscriptions (
 memory_session_id TEXT PRIMARY KEY REFERENCES memory_sessions(id) ON DELETE CASCADE,
 endpoint_encrypted BYTEA NOT NULL,
 keys_encrypted BYTEA NOT NULL,
 interval_minutes INTEGER NOT NULL CHECK (interval_minutes IN (15,30)),
 status TEXT NOT NULL CHECK (status IN ('active','paused')),
 next_send_at TIMESTAMPTZ,
 expires_at TIMESTAMPTZ NOT NULL,
 CHECK ((status='active') = (next_send_at IS NOT NULL))
);
CREATE INDEX push_subscriptions_due ON push_subscriptions(next_send_at) WHERE status='active';
