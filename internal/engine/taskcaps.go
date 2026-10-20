package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// maxTaskCap is the largest value a task's caps JSON may hold for any of
// its keys (§14).
const maxTaskCap = 10

// validateTaskCaps checks a per-task caps override as the foreman names
// it: only fix_rounds, review_rounds and restart, each an integer from 0
// to maxTaskCap. The refusal names the offending key.
func validateTaskCaps(caps map[string]int) error {
	keys := make([]string, 0, len(caps))
	for k := range caps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case roundFix, roundReview, roundRestart:
		default:
			return fmt.Errorf("caps: unknown key %q; want %s, %s or %s", k, roundFix, roundReview, roundRestart)
		}
		if v := caps[k]; v < 0 || v > maxTaskCap {
			return fmt.Errorf("caps: %s is %d; want an integer from 0 to %d", k, v, maxTaskCap)
		}
	}
	return nil
}

// parseTaskCaps reads a tasks.caps column; a value that is not a caps
// object reads as no overrides, as roundLimitTx treats it.
func parseTaskCaps(capsJSON string) map[string]int {
	var caps map[string]int
	if json.Unmarshal([]byte(capsJSON), &caps) != nil || len(caps) == 0 {
		return nil
	}
	return caps
}

// taskCapsJSON is the tasks.caps column for a validated override: '{}'
// when it names nothing.
func taskCapsJSON(caps map[string]int) string {
	if len(caps) == 0 {
		return "{}"
	}
	b, err := json.Marshal(caps) // map keys marshal sorted
	if err != nil {
		return "{}"
	}
	return string(b)
}

// mergeTaskCaps folds an override over a task's recorded caps JSON, key
// by key, and returns the merged column value. A recorded value that is
// not a caps object counts as empty.
func mergeTaskCaps(recorded string, override map[string]int) string {
	merged := map[string]int{}
	_ = json.Unmarshal([]byte(recorded), &merged)
	for k, v := range override {
		merged[k] = v
	}
	return taskCapsJSON(merged)
}

// TaskCapsLabel renders a task's non-default caps compactly, as status
// shows them next to tier and effort: "fix 3, review 1, restarts 4".
// It is empty when the task overrides nothing.
func TaskCapsLabel(caps map[string]int) string {
	var parts []string
	for _, p := range []struct{ key, label string }{{roundFix, "fix"}, {roundReview, "review"}, {roundRestart, "restarts"}} {
		if v, ok := caps[p.key]; ok {
			parts = append(parts, fmt.Sprintf("%s %d", p.label, v))
		}
	}
	return strings.Join(parts, ", ")
}
