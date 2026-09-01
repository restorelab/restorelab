CREATE TABLE api_sessions (
    id         text PRIMARY KEY,
    hash       text NOT NULL UNIQUE,
    token_id   text NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    created_at text NOT NULL,
    expires_at text NOT NULL,
    user_agent text
);

CREATE INDEX api_sessions_expires_at ON api_sessions (expires_at);
