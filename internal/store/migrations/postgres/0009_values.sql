-- What a drill measured, one row per captured value.
--
-- A table rather than a field inside the check's details JSON, because drift
-- has to query across runs, ordered and limited. A JSON blob is where values
-- go to become unqueryable, and reading them back is the whole point.
--
-- The workload is not duplicated here: a value belongs to a check of a run,
-- and the run already knows whose it is. Two places recording the same fact
-- is two places to disagree.
--
-- double precision is written once and works on both engines: PostgreSQL
-- takes it literally, SQLite gives it REAL affinity. An integer column would
-- have looked adequate for a row count and truncated the first duration,
-- ratio or size anybody measured.
CREATE TABLE check_values (
    run_id    text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    check_seq integer NOT NULL,
    name      text NOT NULL,
    value     double precision NOT NULL,
    PRIMARY KEY (run_id, check_seq, name)
);

-- The lookup drift performs, which is by capture name across runs. The
-- workload and the check name are reached by joining runs and run_checks,
-- both of which are already indexed on what that join needs.
CREATE INDEX check_values_lookup ON check_values (name, run_id);
