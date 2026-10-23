-- Execute as trace_owner in the trace database after EVERY reviewed migration.
-- Create trace_app first and set its password interactively with psql \password.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CONNECT ON DATABASE trace TO trace_app;
GRANT USAGE ON SCHEMA public TO trace_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON
  memory_sessions, live_presence_sessions, experiences,
  experience_links, idempotency_records, photos, photo_upload_fences,
  arrival_events, arrival_receipts, arrival_operator_audit TO trace_app;
GRANT SELECT ON schema_migrations TO trace_app;
-- Add explicit grants here for new runtime tables introduced by later migrations;
-- run this file and verify trace_app access before promoting the new app.
-- No table ownership, schema CREATE, or migration-table write permissions for the API.
