ALTER TABLE runs ADD COLUMN queued_at           text;
ALTER TABLE runs ADD COLUMN lease_owner         text;
ALTER TABLE runs ADD COLUMN lease_expires_at    text;
ALTER TABLE runs ADD COLUMN cancel_requested_at text;

CREATE INDEX runs_queue ON runs (state, queued_at);

ALTER TABLE api_tokens ADD COLUMN scopes text NOT NULL DEFAULT 'read';
