-- Existing sessions retain the cloud-photo contract; new registrations opt in explicitly.
ALTER TABLE memory_sessions ADD COLUMN photo_mode TEXT NOT NULL DEFAULT 'legacy'
 CHECK (photo_mode IN ('legacy','device'));

-- Metadata only: image content, image hashes, dimensions, locations and object keys stay on the device.
CREATE TABLE photo_moments (
 id TEXT PRIMARY KEY CHECK (id <> ''),
 id_hash TEXT NOT NULL UNIQUE CHECK (id_hash <> ''),
 memory_session_id TEXT NOT NULL REFERENCES memory_sessions(id) ON DELETE CASCADE,
 client_photo_id TEXT NOT NULL CHECK (client_photo_id <> ''),
 request_hash TEXT NOT NULL CHECK (request_hash <> ''),
 checkpoint_id TEXT NOT NULL CHECK (checkpoint_id <> ''),
 campus_graph_version TEXT NOT NULL CHECK (campus_graph_version <> ''),
 captured_at TIMESTAMPTZ NOT NULL,
 sequence_no INTEGER NOT NULL CHECK (sequence_no > 0),
 UNIQUE (memory_session_id,client_photo_id),
 UNIQUE (memory_session_id,sequence_no)
);
CREATE INDEX ON photo_moments(memory_session_id,captured_at,sequence_no);

-- Retained until memory_sessions.delete_after through its cascading FK.
CREATE TABLE photo_moment_fences (
 memory_session_id TEXT NOT NULL REFERENCES memory_sessions(id) ON DELETE CASCADE,
 client_photo_id TEXT NOT NULL CHECK (client_photo_id <> ''),
 PRIMARY KEY (memory_session_id,client_photo_id)
);

-- Separate from legacy route_segments: its FK must keep referring to old photos.
CREATE TABLE moment_route_segments (
 id TEXT PRIMARY KEY CHECK (id <> ''),
 memory_session_id TEXT NOT NULL REFERENCES memory_sessions(id) ON DELETE CASCADE,
 from_photo_moment_id TEXT NOT NULL REFERENCES photo_moments(id) ON DELETE CASCADE,
 to_photo_moment_id TEXT REFERENCES photo_moments(id) ON DELETE CASCADE,
 coordinate_version TEXT NOT NULL CHECK (coordinate_version <> ''),
 campus_graph_version TEXT NOT NULL CHECK (campus_graph_version <> ''),
 three_d_map_version TEXT NOT NULL CHECK (three_d_map_version <> ''),
 algorithm_version TEXT NOT NULL CHECK (algorithm_version <> ''),
 confidence TEXT NOT NULL CHECK (confidence IN ('high','medium','low')),
 path JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(path) = 'array'),
 path_node_ids JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(path_node_ids) = 'array'),
 length_m DOUBLE PRECISION NOT NULL CHECK (length_m >= 0 AND length_m < 'Infinity'::DOUBLE PRECISION),
 CHECK (from_photo_moment_id <> to_photo_moment_id),
 UNIQUE (memory_session_id,from_photo_moment_id,to_photo_moment_id)
);
CREATE INDEX ON moment_route_segments(memory_session_id,from_photo_moment_id);
