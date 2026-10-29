CREATE TABLE operator_credentials (
 id TEXT PRIMARY KEY,
 token_hash TEXT NOT NULL UNIQUE,
 actor_id TEXT NOT NULL,
 email_hash TEXT NOT NULL,
 origin TEXT NOT NULL,
 role TEXT NOT NULL CHECK (role='operator'),
 created_at TIMESTAMPTZ NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 CHECK (expires_at > created_at AND expires_at <= created_at + INTERVAL '2 minutes')
);
CREATE INDEX operator_credentials_actor ON operator_credentials(actor_id);
CREATE INDEX operator_credentials_expires ON operator_credentials(expires_at);
-- Historical audits retain their original actor with no invented identity mapping.
ALTER TABLE arrival_operator_audit ADD COLUMN operator_email_hash TEXT;
