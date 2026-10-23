CREATE TABLE arrival_events (
 id TEXT PRIMARY KEY,
 memory_session_id TEXT NOT NULL UNIQUE REFERENCES memory_sessions(id) ON DELETE CASCADE,
 source TEXT NOT NULL CHECK (source='qr'),
 checkpoint_id TEXT NOT NULL,
 replay_status TEXT NOT NULL CHECK (replay_status IN ('queued','playing','completed','skipped')),
 skip_reason TEXT CHECK (skip_reason IN ('participant_request','timeout','renderer_failure','operator','queue_full','deleted')),
 arrived_at TIMESTAMPTZ NOT NULL,
 photo_wait_until TIMESTAMPTZ NOT NULL,
 replay_deadline TIMESTAMPTZ NOT NULL,
 replay_started_at TIMESTAMPTZ,
 replay_finished_at TIMESTAMPTZ,
 CHECK (replay_deadline > arrived_at),
 CHECK (photo_wait_until >= arrived_at AND photo_wait_until <= replay_deadline)
);
CREATE INDEX arrival_events_queue ON arrival_events(arrived_at, id) WHERE replay_status='queued';
CREATE INDEX arrival_events_deadline ON arrival_events(replay_deadline) WHERE replay_status='queued';
CREATE TABLE arrival_receipts (
 memory_session_id TEXT NOT NULL REFERENCES memory_sessions(id) ON DELETE CASCADE,
 key_hash TEXT NOT NULL,
 request_hash TEXT NOT NULL,
 response_body BYTEA NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY (memory_session_id,key_hash)
);
CREATE INDEX arrival_receipts_expires ON arrival_receipts(expires_at);
CREATE TABLE arrival_operator_audit (
 id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 operator_id TEXT NOT NULL,
 action TEXT NOT NULL,
 target_id_short TEXT NOT NULL,
 result TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL
);
