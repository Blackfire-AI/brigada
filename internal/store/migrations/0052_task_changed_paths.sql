-- 0052_task_changed_paths: the land gate's protected-path rule on the
-- real paths the task changed (§6.3, §7.1). The runner records them
-- (`git diff --name-only base...branch`) in the work job's result when
-- it finishes; the engine stores them with the task, a JSON array, and
-- the land gate matches them instead of the spec+check proxy. An older
-- runner that reports none leaves the column empty, and the proxy
-- stands in as before.

ALTER TABLE tasks ADD COLUMN changed_paths TEXT NOT NULL DEFAULT '';
