CREATE TABLE route_segments (
 id TEXT PRIMARY KEY CHECK (id <> ''),
 memory_session_id TEXT NOT NULL REFERENCES memory_sessions(id) ON DELETE CASCADE,
 from_photo_moment_id TEXT NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
 to_photo_moment_id TEXT NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
 coordinate_version TEXT NOT NULL CHECK (coordinate_version <> ''),
 campus_graph_version TEXT NOT NULL CHECK (campus_graph_version <> ''),
 three_d_map_version TEXT NOT NULL CHECK (three_d_map_version <> ''),
 algorithm_version TEXT NOT NULL CHECK (algorithm_version <> ''),
 confidence TEXT NOT NULL CHECK (confidence IN ('high','medium','low')),
 path JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(path) = 'array'),
 length_m DOUBLE PRECISION NOT NULL CHECK (length_m >= 0 AND length_m < 'Infinity'::DOUBLE PRECISION),
 created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
 UNIQUE (memory_session_id,from_photo_moment_id,to_photo_moment_id),
 CHECK (from_photo_moment_id <> to_photo_moment_id)
);
CREATE INDEX ON route_segments(memory_session_id,from_photo_moment_id);
