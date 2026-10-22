-- The plan budget (§6.3) sums a plan's recorded job usage through its
-- tasks; the ready-wave, amendment and dependent scans read a plan's
-- tasks by state the same way.
CREATE INDEX idx_tasks_plan_state ON tasks(plan_id, state);
