package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The environment step of a task (§5.7): a command the runner runs in the
// worktree before the worker and again before the check, with a cache
// directory keyed by the files the project names.

// defaultSetupTimeoutMinutes bounds one setup run when the project sets no
// bound of its own (§5.7, open in §16 item 8).
const defaultSetupTimeoutMinutes = 15

// ErrorKindSetup sorts a completion's Error as a setup failure (§5.7): an
// infrastructure failure of the job, neither a failed check nor a worker
// failure.
const ErrorKindSetup = "setup"

// setupRetries is how many times a failed setup requeues the task's work
// before the task goes to the foreman (§5.7: retried like a transient
// error, capped).
const setupRetries = 2

// SetupSpec is the project's setup as a task records it (§5.7): the
// command, the repo-relative files whose contents key the cache, the time
// bound, and where the setting came from (its source and the hash of the
// imported file), so a later change to the project setting leaves a
// recorded task alone.
type SetupSpec struct {
	Command        string   `json:"command"`
	CacheKeyFiles  []string `json:"cache_key_files,omitempty"`
	TimeoutMinutes int      `json:"timeout_minutes"`
	Source         string   `json:"source,omitempty"`
	Hash           string   `json:"hash,omitempty"`
}

// projectSetupTx reads the project's effective setup; nil when it sets
// none.
func projectSetupTx(tx *sql.Tx, project int64) (*SetupSpec, error) {
	var raw, rawProv string
	if err := tx.QueryRow("SELECT settings, settings_prov FROM projects WHERE id = ?", project).Scan(&raw, &rawProv); err != nil {
		return nil, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	return setupFromSettings(raw, rawProv, project)
}

func setupFromSettings(raw, rawProv string, project int64) (*SetupSpec, error) {
	if raw == "" {
		return nil, nil
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &present); err != nil {
		return nil, fmt.Errorf("parse the settings of project %d: %w", project, err)
	}
	cmd, _ := rawString(present[SettingSetup])
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil, nil
	}
	prov, err := parseProv(rawProv)
	if err != nil {
		return nil, fmt.Errorf("parse the provenance of project %d: %w", project, err)
	}
	spec := &SetupSpec{Command: cmd, TimeoutMinutes: defaultSetupTimeoutMinutes, Source: SourceLocal}
	var files stringList
	if v, ok := present[SettingSetupCacheKeyFiles]; ok {
		if err := json.Unmarshal(v, &files); err != nil {
			return nil, fmt.Errorf("parse %s of project %d: %w", SettingSetupCacheKeyFiles, project, err)
		}
		spec.CacheKeyFiles = files
	}
	if v, ok := present[SettingSetupTimeout]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil && n > 0 {
			spec.TimeoutMinutes = n
		}
	}
	if prov.isImported(SettingSetup, present[SettingSetup]) {
		spec.Source, spec.Hash = SourceImported, prov.Hash
	}
	return spec, nil
}

// setupRecord renders a spec for the tasks.setup column; "" for none.
func setupRecord(spec *SetupSpec) string {
	if spec == nil {
		return ""
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseSetupRecord reads a tasks.setup column; nil for none or for a
// value that does not parse.
func parseSetupRecord(s string) *SetupSpec {
	if s == "" {
		return nil
	}
	var spec SetupSpec
	if json.Unmarshal([]byte(s), &spec) != nil || strings.TrimSpace(spec.Command) == "" {
		return nil
	}
	if spec.TimeoutMinutes <= 0 {
		spec.TimeoutMinutes = defaultSetupTimeoutMinutes
	}
	return &spec
}

// taskProjectSetupTx resolves the setup a task created now would record:
// the project of the plan it belongs to.
func taskProjectSetupTx(tx *sql.Tx, project int64) (string, error) {
	spec, err := projectSetupTx(tx, project)
	if err != nil {
		return "", err
	}
	return setupRecord(spec), nil
}

// taskSetupTx reads the setup a task recorded at creation.
func taskSetupTx(tx *sql.Tx, taskID int64) (*SetupSpec, error) {
	var rec sql.NullString
	err := tx.QueryRow("SELECT setup FROM tasks WHERE id = ?", taskID).Scan(&rec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the setup of task %d: %w", taskID, err)
	}
	return parseSetupRecord(rec.String), nil
}

// validateCacheKeyFile refuses a cache key file that is not a repo-relative
// path: the runner reads it from the worktree, and a path out of it would
// hash the host.
func validateCacheKeyFile(item string) error {
	if strings.HasPrefix(item, "/") || strings.HasPrefix(item, "~") || strings.Contains(item, "\\") {
		return fmt.Errorf("%s item %q must be a repo-relative path", SettingSetupCacheKeyFiles, item)
	}
	for _, part := range strings.Split(item, "/") {
		if part == ".." {
			return fmt.Errorf("%s item %q must stay inside the repository", SettingSetupCacheKeyFiles, item)
		}
	}
	return nil
}

// setupRetryCountTx counts the task's jobs that failed on a setup.
func setupRetryCountTx(tx *sql.Tx, taskID int64) (int, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE task_id = ? AND status = 'failed'
		AND result LIKE '%"error":"setup failed%'`, taskID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count the setup failures of task %d: %w", taskID, err)
	}
	return n, nil
}
