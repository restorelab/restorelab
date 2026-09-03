-- What a run established: NONE, BOOT, SERVICE or DATA.
--
-- NULL means the run predates this column, and that is not the same as NONE:
-- it means "not recorded", and nothing may be concluded from it. The
-- confidence score reads it as unknown and caps nothing, rather than
-- retroactively punishing every workload for a fact nobody ever wrote down.
ALTER TABLE runs ADD COLUMN proof_level text;
