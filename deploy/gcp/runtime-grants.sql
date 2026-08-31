-- Execute as trace_owner, in the trace database, AFTER the initial migration.
-- Create trace_app first and set its password interactively with psql \password.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CONNECT ON DATABASE trace TO trace_app;
GRANT USAGE ON SCHEMA public TO trace_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON
  memory_sessions, live_presence_sessions, experiences,
  experience_links, live_positions, idempotency_records TO trace_app;
GRANT SELECT ON schema_migrations TO trace_app;
-- No table ownership, schema CREATE, or migration-table write permissions for the API.
