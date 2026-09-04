-- When this run was considered for a notification.
--
-- It is a claim, not a record of success: one dispatcher wins the
-- "UPDATE ... WHERE notified_at IS NULL" and then decides whether the run is
-- worth a message at all. Most runs are not, and stay silent with the column
-- set.
--
-- A nullable column rather than a cursor over (completed_at, id) on purpose:
-- a run that finishes late and is written after a cursor has passed its
-- timestamp would never be looked at, and the drill nobody hears about would
-- be the one that behaved unusually.
ALTER TABLE runs ADD COLUMN notified_at text;

-- Every run that already exists has been considered, and none is worth a
-- message now.
--
-- E1 refused to backfill proof_level, and this is the same rule rather than an
-- exception to it: inventing a proof level would assert something false about
-- a drill, while this asserts something true about RestoreLab. Without it, the
-- first tick after an upgrade replays the entire history into somebody's chat
-- channel.
UPDATE runs SET notified_at = COALESCE(completed_at, started_at);

CREATE INDEX runs_unnotified ON runs (completed_at) WHERE notified_at IS NULL;

CREATE TABLE notification_deliveries (
    id         text PRIMARY KEY,
    run_id     text NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    channel_id text NOT NULL,
    kind       text NOT NULL,
    state      text NOT NULL,
    attempts   integer NOT NULL DEFAULT 0,
    next_at    text,
    status     integer NOT NULL DEFAULT 0,
    error      text,
    payload    text NOT NULL,
    created_at text NOT NULL,
    sent_at    text
);

-- One delivery per run per channel. Adding a channel does not re-announce old
-- runs, and a dispatcher restarted mid-flight cannot double-post to the same
-- place.
CREATE UNIQUE INDEX notification_deliveries_once
    ON notification_deliveries (run_id, channel_id);

CREATE INDEX notification_deliveries_due ON notification_deliveries (state, next_at);
