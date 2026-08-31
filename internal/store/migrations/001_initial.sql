CREATE TABLE memory_sessions (
 id TEXT PRIMARY KEY, consent_version TEXT NOT NULL, consent_language TEXT NOT NULL,
 token_hash TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
 delete_after TIMESTAMPTZ NOT NULL CHECK (delete_after >= expires_at)
);
CREATE TABLE live_presence_sessions (
 id TEXT PRIMARY KEY, consent_version TEXT NOT NULL, on_site_consent_version TEXT, token_hash TEXT NOT NULL,
 tracking_status TEXT NOT NULL CHECK (tracking_status IN ('tracking','stopped','expired')),
 presence_phase TEXT NOT NULL CHECK (presence_phase IN ('approaching','arrived','on_site','ended')),
 stop_reason TEXT, display_color TEXT NOT NULL, display_symbol TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL, stopped_at TIMESTAMPTZ, expires_at TIMESTAMPTZ NOT NULL,
 delete_after TIMESTAMPTZ NOT NULL CHECK (delete_after >= expires_at), last_seen_at TIMESTAMPTZ
);
CREATE TABLE experiences (
 id TEXT PRIMARY KEY,
 memory_session_id TEXT NOT NULL UNIQUE REFERENCES memory_sessions(id) ON DELETE CASCADE,
 live_session_id TEXT UNIQUE REFERENCES live_presence_sessions(id) ON DELETE SET NULL,
 display_color TEXT NOT NULL, display_symbol TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL, delete_after TIMESTAMPTZ NOT NULL
);
CREATE TABLE experience_links (
 id TEXT PRIMARY KEY,
 memory_session_id TEXT NOT NULL UNIQUE REFERENCES memory_sessions(id) ON DELETE CASCADE,
 live_session_id TEXT NOT NULL UNIQUE REFERENCES live_presence_sessions(id) ON DELETE CASCADE,
 token_hash TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE live_positions (
 live_session_id TEXT PRIMARY KEY REFERENCES live_presence_sessions(id) ON DELETE CASCADE,
 accuracy_m DOUBLE PRECISION NOT NULL CHECK (accuracy_m > 0),
 local_x DOUBLE PRECISION NOT NULL, local_z DOUBLE PRECISION NOT NULL,
 recorded_at TIMESTAMPTZ NOT NULL, received_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE idempotency_records (
 scope TEXT NOT NULL, key_hash TEXT NOT NULL, request_hash TEXT NOT NULL,
 response_status INTEGER NOT NULL, response_body BYTEA NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY (scope,key_hash)
);
CREATE INDEX ON memory_sessions(delete_after);
CREATE INDEX ON live_presence_sessions(expires_at) WHERE tracking_status='tracking';
CREATE INDEX ON live_presence_sessions(delete_after);
CREATE INDEX ON experiences(delete_after);
CREATE INDEX ON experience_links(expires_at);
CREATE INDEX ON live_positions(expires_at);
CREATE INDEX ON idempotency_records(expires_at);
