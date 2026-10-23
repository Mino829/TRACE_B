-- The cursor row serializes both event commits and snapshot cuts across revisions.
CREATE TABLE display_cursor (singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton), last_id BIGINT NOT NULL DEFAULT 0);
INSERT INTO display_cursor (singleton,last_id) VALUES (TRUE,0);
ALTER TABLE live_presence_sessions ADD COLUMN display_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE;
CREATE TABLE display_events (
 id BIGINT PRIMARY KEY, kind TEXT NOT NULL, data JSONB NOT NULL, occurred_at TIMESTAMPTZ NOT NULL
);
ALTER TABLE arrival_events ADD COLUMN renderer_lease_id TEXT;
CREATE INDEX display_events_expiry ON display_events(occurred_at);
CREATE TABLE display_primary_lease (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
 lease_id TEXT, holder_id TEXT, expires_at TIMESTAMPTZ
);
INSERT INTO display_primary_lease(singleton) VALUES (TRUE);
CREATE TABLE display_renderer_receipts (
 event_id TEXT PRIMARY KEY, lease_id TEXT NOT NULL, kind TEXT NOT NULL, arrival_id TEXT NOT NULL,
 result TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX display_renderer_receipts_expiry ON display_renderer_receipts(created_at);
