ALTER TABLE photos ADD COLUMN checkpoint_id TEXT CHECK (checkpoint_id IS NULL OR checkpoint_id <> '');
ALTER TABLE photos ADD COLUMN campus_graph_version TEXT CHECK (campus_graph_version IS NULL OR campus_graph_version <> '');
ALTER TABLE memory_sessions ADD COLUMN campus_graph_version TEXT CHECK (campus_graph_version IS NULL OR campus_graph_version <> '');
-- The arrival segment ends at the arrival checkpoint instead of another photo.
ALTER TABLE route_segments ALTER COLUMN to_photo_moment_id DROP NOT NULL;
ALTER TABLE route_segments ADD COLUMN path_node_ids JSONB NOT NULL DEFAULT '[]'::jsonb
 CHECK (jsonb_typeof(path_node_ids) = 'array');
