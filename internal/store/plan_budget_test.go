package store

import (
	"path/filepath"
	"testing"
)

func TestPlanBudgetMigration(t *testing.T) {
	migs, err := allMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if migs[56].version != 57 || migs[56].name != "plan_budget" {
		t.Fatal("the plan_budget index must be migration 0057")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	st := openStore(t, path)
	defer st.Close()
	if err := st.db.Ping(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_tasks_plan_state'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("the store opened without the idx_tasks_plan_state index")
	}
}
