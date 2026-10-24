-- Marker configuration is operator-controlled and disabled until approved values are supplied.
CREATE TABLE marker_control (
 id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
 pins_enabled BOOLEAN NOT NULL DEFAULT FALSE,
 hotspots_enabled BOOLEAN NOT NULL DEFAULT FALSE,
 sparse_override BOOLEAN NOT NULL DEFAULT TRUE,
 coordinate_version TEXT NOT NULL DEFAULT '',
 graph_version TEXT NOT NULL DEFAULT '',
 three_d_version TEXT NOT NULL DEFAULT '',
 width_m DOUBLE PRECISION NOT NULL DEFAULT 0,
 ttl_seconds INTEGER NOT NULL DEFAULT 0,
 sparse_min_sessions INTEGER NOT NULL DEFAULT 0
);
INSERT INTO marker_control(id) VALUES (TRUE);

-- Photo identity and session membership remain private; never select this table for display.
CREATE TABLE marker_photos (
 photo_id TEXT PRIMARY KEY REFERENCES photos(id) ON DELETE CASCADE,
 public_id TEXT NOT NULL UNIQUE,
 bucket_x INTEGER NOT NULL,
 bucket_z INTEGER NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX ON marker_photos(expires_at);
CREATE TABLE marker_bucket_ids (
 bucket_x INTEGER NOT NULL,
 bucket_z INTEGER NOT NULL,
 public_id TEXT NOT NULL UNIQUE,
 PRIMARY KEY(bucket_x,bucket_z)
);

-- Public materialization and tombstones contain only quantized coordinates and opaque IDs.
CREATE TABLE marker_visible (
 kind TEXT NOT NULL CHECK (kind IN ('pin','hotspot')),
 public_id TEXT NOT NULL,
 x DOUBLE PRECISION NOT NULL,
 z DOUBLE PRECISION NOT NULL,
 PRIMARY KEY(kind,public_id)
);
