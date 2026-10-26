ALTER TABLE memory_sessions ADD COLUMN campus_graph_version TEXT CHECK (campus_graph_version IS NULL OR campus_graph_version <> '');
ALTER TABLE route_segments ADD COLUMN path_node_ids JSONB NOT NULL DEFAULT '[]'::jsonb
 CHECK (jsonb_typeof(path_node_ids) = 'array');
