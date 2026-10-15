-- 0023_review_without_task: a standalone review (§7, `brigada review
-- --branch`) is a review job that belongs to no plan and no task. 0001
-- paired kind and task in both directions — a foreman job without a task,
-- every worker job with one — so the CHECK on jobs is relaxed to let a
-- review job go without one. SQLite cannot alter a CHECK in place; the
-- edit itself runs in Go (relaxJobsTaskCheck in migrate.go), as the
-- ALTER TABLE documentation prescribes for changing a constraint: the
-- table's definition is edited in place, nothing is rebuilt, so no row,
-- index or foreign key reference moves.
SELECT 1;
