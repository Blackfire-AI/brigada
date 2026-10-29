package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// taskSkills decodes the skills column a task recorded (§5.10), in the
// order given; an empty or unreadable column is no skills.
func taskSkills(raw string) []TaskSkill {
	if raw == "" {
		return nil
	}
	var out []TaskSkill
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// taskSkillNames is the names of a task's recorded skills, in order; nil
// when it carries none.
func taskSkillNames(raw string) []string {
	var names []string
	for _, s := range taskSkills(raw) {
		names = append(names, s.Name)
	}
	return names
}

// taskSkillCountTx is how many skills a task recorded: the key the
// outcome record splits on, so a one-skill and a two-skill task are not
// pooled (§8).
func taskSkillCountTx(tx *sql.Tx, taskID int64) (int, error) {
	if taskID == 0 {
		return 0, nil // a standalone review has no task
	}
	var raw string
	if err := tx.QueryRow("SELECT skills FROM tasks WHERE id = ?", taskID).Scan(&raw); err != nil {
		return 0, fmt.Errorf("read the skills of task %d: %w", taskID, err)
	}
	return len(taskSkills(raw)), nil
}
