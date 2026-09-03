CREATE TABLE schedule_slots (
    plan_id    text NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    slot_at    text NOT NULL,
    decided_at text NOT NULL,
    outcome    text NOT NULL,
    reason     text,
    run_id     text REFERENCES runs(id) ON DELETE SET NULL,
    PRIMARY KEY (plan_id, slot_at)
);

CREATE INDEX schedule_slots_recent ON schedule_slots (plan_id, slot_at DESC);
