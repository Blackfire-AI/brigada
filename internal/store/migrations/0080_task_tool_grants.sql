-- 0080_task_tool_grants: the task's per-task tool grants (§6.1):
-- tasks.tool_grants holds a JSON array of Claude Code permission
-- patterns drawn from the binary's allowlist (engine.ToolGrantAllowlist),
-- '' for a task that grants none. The patterns are prefix rules, not a
-- security boundary — the pipeline's structure is (§5.2). Every task
-- that predates the field grants none.
ALTER TABLE tasks ADD COLUMN tool_grants TEXT NOT NULL DEFAULT '';
