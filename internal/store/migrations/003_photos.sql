ALTER TABLE memory_sessions ADD COLUMN deleting BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE memory_sessions ADD COLUMN photo_sequence INTEGER NOT NULL DEFAULT 0 CHECK (photo_sequence >= 0);

CREATE TABLE photos (
 id TEXT PRIMARY KEY CHECK (id <> ''),
 id_hash TEXT NOT NULL UNIQUE CHECK (id_hash <> ''),
 memory_session_id TEXT NOT NULL REFERENCES memory_sessions(id) ON DELETE RESTRICT,
 client_photo_id TEXT NOT NULL CHECK (client_photo_id <> ''),
 request_hash TEXT NOT NULL CHECK (request_hash <> ''),
 captured_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 delete_after TIMESTAMPTZ NOT NULL,
 upload_expires_at TIMESTAMPTZ NOT NULL,
 latitude DOUBLE PRECISION,
 longitude DOUBLE PRECISION,
 accuracy_m DOUBLE PRECISION,
 location_recorded_at TIMESTAMPTZ,
 mime_type TEXT NOT NULL CHECK (mime_type IN ('image/jpeg','image/webp')),
 sha256 TEXT NOT NULL,
 size_bytes BIGINT NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 5242880),
 width INTEGER NOT NULL CHECK (width > 0 AND width <= 1920),
 height INTEGER NOT NULL CHECK (height > 0 AND height <= 1920),
 sequence_no INTEGER NOT NULL CHECK (sequence_no > 0),
 upload_attempts INTEGER NOT NULL CHECK (upload_attempts BETWEEN 1 AND 5),
 status TEXT NOT NULL CHECK (status IN ('pending','ready','failed','deleting')),
 stage_key TEXT UNIQUE,
 object_key TEXT UNIQUE,
 failure_code TEXT NOT NULL DEFAULT '',
 delete_target TEXT NOT NULL DEFAULT '',
 UNIQUE (memory_session_id,client_photo_id),
 UNIQUE (memory_session_id,sequence_no),
 CHECK (captured_at <= delete_after AND created_at <= delete_after),
 CHECK (upload_expires_at <= delete_after),
 CHECK ((latitude IS NULL AND longitude IS NULL AND accuracy_m IS NULL AND location_recorded_at IS NULL)
     OR (latitude IS NOT NULL AND longitude IS NOT NULL AND accuracy_m IS NOT NULL AND location_recorded_at IS NOT NULL
         AND latitude BETWEEN -90 AND 90 AND longitude BETWEEN -180 AND 180 AND accuracy_m > 0 AND accuracy_m < 'Infinity'::DOUBLE PRECISION)),
 CHECK ((status = 'deleting' AND delete_target IN ('deleted','failed')) OR (status <> 'deleting' AND delete_target = '')),
 CHECK ((status = 'failed' AND stage_key IS NULL AND object_key IS NULL)
     OR (status <> 'failed' AND stage_key IS NOT NULL AND stage_key <> '' AND object_key IS NOT NULL AND object_key <> '' AND stage_key <> object_key)
     OR (status = 'deleting' AND stage_key IS NULL AND object_key IS NULL))
);
CREATE INDEX ON photos(delete_after);
CREATE INDEX ON photos(memory_session_id,sequence_no);
CREATE INDEX ON photos(memory_session_id,id) WHERE status='deleting';

-- No session foreign key: late signed uploads remain fenced after metadata erasure.
CREATE TABLE photo_upload_fences (
 object_key TEXT PRIMARY KEY CHECK (object_key <> ''),
 not_before TIMESTAMPTZ NOT NULL
);
CREATE INDEX ON photo_upload_fences(not_before);
