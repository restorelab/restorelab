CREATE TABLE api_tokens (
    id           text PRIMARY KEY,
    name         text NOT NULL UNIQUE,
    hash         text NOT NULL UNIQUE,
    created_at   text NOT NULL,
    last_used_at text,
    revoked_at   text
);
