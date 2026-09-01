CREATE TABLE plans (
    id          text    PRIMARY KEY,
    name        text    NOT NULL UNIQUE,
    description text,
    workload_id text    NOT NULL,
    provider_id text,
    plan_yaml   text    NOT NULL,
    version     integer NOT NULL DEFAULT 1,
    created_at  text    NOT NULL,
    updated_at  text    NOT NULL
);

CREATE INDEX plans_workload ON plans (workload_id);

ALTER TABLE runs ADD COLUMN plan_id      text REFERENCES plans(id) ON DELETE SET NULL;
ALTER TABLE runs ADD COLUMN plan_version integer;
