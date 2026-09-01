CREATE TABLE runs (
    id                  text    PRIMARY KEY,
    plan_name           text    NOT NULL,
    plan_snapshot       text    NOT NULL,
    provider_id         text    NOT NULL,
    backup_provider_id  text,
    source_workload_id  text    NOT NULL,
    source_name         text,
    temp_workload_id    text,
    temp_name           text,
    node                text,
    backup              text,
    state               text    NOT NULL,
    result              text,
    started_at          text    NOT NULL,
    completed_at        text,
    rto_ms              integer,
    rto_target_ms       integer,
    cleanup_done        integer NOT NULL DEFAULT 0,
    err                 text
);

CREATE INDEX runs_source_started ON runs (source_workload_id, started_at DESC);
CREATE INDEX runs_started ON runs (started_at DESC);

CREATE TABLE run_steps (
    run_id       text    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq          integer NOT NULL,
    name         text    NOT NULL,
    state        text    NOT NULL,
    status       text    NOT NULL,
    started_at   text,
    completed_at text,
    duration_ms  integer,
    message      text,
    err          text,
    details      text,
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE run_checks (
    run_id       text    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq          integer NOT NULL,
    name         text    NOT NULL,
    type         text    NOT NULL,
    status       text    NOT NULL,
    started_at   text,
    completed_at text,
    duration_ms  integer,
    attempts     integer,
    message      text,
    details      text,
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE run_events (
    run_id       text    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    seq          integer NOT NULL,
    at           text    NOT NULL,
    state        text    NOT NULL,
    step         text,
    status       text,
    message      text,
    check_result text,
    err          text,
    PRIMARY KEY (run_id, seq)
);
