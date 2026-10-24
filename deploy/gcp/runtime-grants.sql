-- Execute as trace_owner in the trace database after EVERY reviewed migration.
-- Create trace_app first and set its password interactively with psql \password.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CONNECT ON DATABASE trace TO trace_app;
GRANT USAGE ON SCHEMA public TO trace_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON
  memory_sessions, live_presence_sessions, experiences,
  experience_links, idempotency_records, photos, photo_upload_fences, push_subscriptions, route_segments,
  arrival_events, arrival_receipts, arrival_operator_audit,
  display_events, display_cursor, display_primary_lease, display_renderer_receipts,
  marker_control, marker_photos, marker_bucket_ids, marker_visible TO trace_app;
GRANT SELECT ON schema_migrations TO trace_app;
-- No table ownership, schema CREATE, or migration-table write permissions for the API.
