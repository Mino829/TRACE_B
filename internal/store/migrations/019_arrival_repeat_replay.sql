ALTER TABLE arrival_events ADD COLUMN replay_id TEXT;
ALTER TABLE arrival_events ADD COLUMN queued_at TIMESTAMPTZ;
UPDATE arrival_events SET replay_id=id, queued_at=arrived_at;
ALTER TABLE arrival_events ALTER COLUMN replay_id SET NOT NULL;
ALTER TABLE arrival_events ALTER COLUMN queued_at SET NOT NULL;
ALTER TABLE arrival_events ADD CONSTRAINT arrival_events_replay_id_unique UNIQUE (replay_id);
DROP INDEX arrival_events_queue;
CREATE INDEX arrival_events_queue ON arrival_events(queued_at,id) WHERE replay_status='queued';

-- Receipts remain recoverable for the lifetime of their owning MemorySession.
CREATE TABLE arrival_replay_receipts (
 memory_session_id TEXT NOT NULL REFERENCES memory_sessions(id) ON DELETE CASCADE,
 key_hash TEXT NOT NULL,
 request_hash TEXT NOT NULL,
 response_body BYTEA NOT NULL,
 PRIMARY KEY (memory_session_id,key_hash)
);
CREATE TABLE arrival_proof_uses (
 memory_session_id TEXT NOT NULL REFERENCES memory_sessions(id) ON DELETE CASCADE,
 proof_fingerprint TEXT NOT NULL CHECK (length(proof_fingerprint)=64),
 PRIMARY KEY (memory_session_id,proof_fingerprint)
);
