package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/planfile"
)

// Plan files in recovery (§6.4): after the store is restored from its
// snapshot, each plan file on the repo's plans branch is applied. A plan
// the snapshot lacks, or holds with an older updated time than its file,
// is rebuilt from the file — id, tasks, names, deps, states, branches and
// open questions. The file is data: it never says more than the store's
// own columns can hold, and once applied the store is authoritative
// again. A plan the snapshot holds as new as its file, or newer, is left
// alone.
//
// What a file cannot carry stays lost: a rebuilt task has no spec and no
// check command, and a task the file shows mid-flight (running, checking,
// reviewing) has no job anymore, so it waits at needs_foreman for the
// foreman to decide.

// Plan recovery's per-plan outcomes.
const (
	PlanRebuilt = "rebuilt" // the plan was absent from the store; built from its file
	PlanUpdated = "updated" // the file was newer than the plan the store held
	PlanKept    = "kept"    // the store's plan is as new as its file, or newer
	PlanSkipped = "skipped" // the file could not be applied; Detail says why
)

// PlanRecovery is one plan file's outcome.
type PlanRecovery struct {
	Repo   string
	PlanID int64
	Action string
	Detail string
}

var (
	planStates = map[string]bool{"draft": true, "pending_approval": true, "active": true, "done": true, "cancelled": true}
	taskStates = map[string]bool{"pending": true, "ready": true, "running": true, "checking": true, "reviewing": true,
		"done": true, "needs_foreman": true, "needs_user": true, "failed": true, "cancelled": true}
)

// RecoverPlans applies the plan files of every project in the store, and
// of each repo named in extraRepos (a repo with no project gets one, its
// foreman unchosen), reading each from the project's plans branch. Fetch
// failures and unreadable files come back as warnings: recovery rebuilds
// what it can.
func (e *Engine) RecoverPlans(ctx context.Context, extraRepos []string) (results []PlanRecovery, warnings []string, err error) {
	type project struct {
		id                                      int64
		repo, transport, defaultBranch, setting string
	}
	var projects []project
	// A restored snapshot may hold identities in an older form: bring
	// them to the canonical one before any lookup (idempotent).
	if _, rerr := e.CanonicalizeProjects(ctx); rerr != nil {
		return nil, nil, fmt.Errorf("recover plans: %w", rerr)
	}
	err = e.store.Store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		for _, repo := range extraRepos {
			project, err := LookupProjectTx(tx, repo)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				if _, err := createProjectTx(tx, repo, setupDefaultBranch, "", "", now); err != nil {
					if errors.Is(err, ErrInvalidRepoURL) {
						warnings = append(warnings, err.Error())
						continue
					}
					return err
				}
			case err == nil:
				if err := activeProjectTx(tx, project); err != nil {
					return err
				}
			case errors.Is(err, ErrInvalidRepoURL):
				warnings = append(warnings, fmt.Sprintf("%s: %v", RedactRepoURL(repo), err))
			case err != nil:
				return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repo), err)
			}
		}
		rows, err := tx.Query("SELECT id, repo_url, COALESCE(NULLIF(transport_url, ''), repo_url), default_branch, settings FROM projects WHERE archived_at IS NULL ORDER BY id")
		if err != nil {
			return fmt.Errorf("list projects: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p project
			if err := rows.Scan(&p.id, &p.repo, &p.transport, &p.defaultBranch, &p.setting); err != nil {
				return fmt.Errorf("list projects: %w", err)
			}
			projects = append(projects, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, fmt.Errorf("recover plans: %w", err)
	}

	for _, p := range projects {
		var cfg planFileSettings
		if p.setting != "" {
			if err := json.Unmarshal([]byte(p.setting), &cfg); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: parse the project's settings: %v", p.repo, err))
				continue
			}
		}
		target := planFileTarget(p.transport, p.defaultBranch, cfg.Branch)
		fctx, cancel := context.WithTimeout(ctx, e.cfg.PlanFileTimeout)
		fetched, err := planfile.Fetch(fctx, target)
		cancel()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: read the plan files on %s: %v", p.repo, target.Branch, err))
			continue
		}
		for _, s := range fetched.Skipped {
			warnings = append(warnings, fmt.Sprintf("%s: plan file %s", p.repo, s))
		}
		for _, plan := range fetched.Plans {
			res := PlanRecovery{Repo: p.repo, PlanID: plan.ID}
			err := e.store.Store.Update(ctx, func(tx *sql.Tx) error {
				var err error
				res.Action, res.Detail, err = e.applyPlanFileTx(tx, p.id, p.repo, p.defaultBranch, plan, e.cfg.Now())
				return err
			})
			if err != nil {
				return results, warnings, fmt.Errorf("recover plan %d: %w", plan.ID, err)
			}
			results = append(results, res)
		}
	}
	return results, warnings, nil
}

// applyPlanFileTx applies one plan file to the store, in the caller's
// transaction. It checks everything it can refuse before it writes
// anything, so a skipped plan leaves the store as it was.
func (e *Engine) applyPlanFileTx(tx *sql.Tx, project int64, repo, defaultBranch string, p planfile.Plan, now time.Time) (action, detail string, err error) {
	if err := activeProjectTx(tx, project); err != nil {
		return "", "", err
	}
	skip := func(format string, args ...any) (string, string, error) {
		return PlanSkipped, fmt.Sprintf(format, args...), nil
	}
	// A file written before identities were canonical names the repo as it
	// was given; it is the same repo when it canonicalizes to the project's.
	if fileRepo, cerr := Canonicalize(p.Repo); p.Repo != repo && (cerr != nil || fileRepo != repo) {
		return skip("the file names repo %q, not %q", RedactRepoURL(p.Repo), repo)
	}
	if !planStates[p.State] {
		return skip("unknown plan state %q", p.State)
	}

	var storeProject int64
	var storeUpdated string
	exists := true
	switch err := tx.QueryRow("SELECT project_id, updated_at FROM plans WHERE id = ?", p.ID).Scan(&storeProject, &storeUpdated); {
	case errors.Is(err, sql.ErrNoRows):
		exists = false
	case err != nil:
		return "", "", fmt.Errorf("read plan %d: %w", p.ID, err)
	}
	if exists {
		if storeProject != project {
			return skip("plan %d belongs to another project in the store", p.ID)
		}
		have, err := parseTime(storeUpdated)
		if err != nil {
			return "", "", fmt.Errorf("plan %d: updated_at: %w", p.ID, err)
		}
		if !p.Updated.After(have) {
			return PlanKept, "the store's plan is as new as its file", nil
		}
	}

	ids := map[string]int64{}
	for _, t := range p.Tasks {
		if !taskStates[t.State] {
			return skip("task %d: unknown state %q", t.ID, t.State)
		}
		if t.Name != "" {
			if _, dup := ids[t.Name]; dup {
				return skip("task name %q is used twice", t.Name)
			}
			ids[t.Name] = t.ID
		}
		var owner int64
		switch err := tx.QueryRow("SELECT plan_id FROM tasks WHERE id = ?", t.ID).Scan(&owner); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", "", fmt.Errorf("read task %d: %w", t.ID, err)
		case owner != p.ID:
			return skip("task %d belongs to plan %d in the store", t.ID, owner)
		}
	}
	for _, t := range p.Tasks {
		for _, d := range t.Deps {
			if id, ok := ids[d]; !ok || id == t.ID {
				return skip("task %d depends on %q, which is no other task of the plan", t.ID, d)
			}
		}
		if t.Base != "" && !slices.Contains(t.Deps, t.Base) {
			return skip("task %d has base %q, which is not one of its deps", t.ID, t.Base)
		}
	}

	if exists {
		if _, err := tx.Exec("UPDATE plans SET title = ?, state = ?, version = version + 1, updated_at = ? WHERE id = ?",
			p.Title, p.State, fmtTime(p.Updated), p.ID); err != nil {
			return "", "", fmt.Errorf("update plan %d: %w", p.ID, err)
		}
	} else if _, err := tx.Exec(`INSERT INTO plans (id, project_id, title, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, p.ID, project, p.Title, p.State, fmtTime(p.Created), fmtTime(p.Updated)); err != nil {
		return "", "", fmt.Errorf("rebuild plan %d: %w", p.ID, err)
	}

	for _, t := range p.Tasks {
		state := t.State
		switch state {
		case "running", "checking", "reviewing":
			var live bool
			if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM jobs WHERE task_id = ? AND status IN ('queued', 'leased', 'running'))", t.ID).Scan(&live); err != nil {
				return "", "", fmt.Errorf("read the jobs of task %d: %w", t.ID, err)
			}
			if !live {
				state = "needs_foreman"
			}
		}
		var known bool
		if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM tasks WHERE id = ?)", t.ID).Scan(&known); err != nil {
			return "", "", fmt.Errorf("read task %d: %w", t.ID, err)
		}
		if known {
			_, err = tx.Exec("UPDATE tasks SET name = ?, title = ?, class = ?, state = ?, branch = NULLIF(?, ''), tier = ?, effective_tier = ?, effort = ?, effective_effort = ?, caps = ?, updated_at = ? WHERE id = ?",
				t.Name, t.Title, t.Class, state, t.Branch, taskTier(t.Tier), taskTier(t.Tier), t.Effort, t.Effort, taskCapsJSON(t.Caps), fmtTime(p.Updated), t.ID)
		} else {
			_, err = tx.Exec(`INSERT INTO tasks (id, plan_id, name, title, spec, check_cmd, class, state, base_ref, branch, caps, tier, effective_tier, effort, effective_effort, created_at, updated_at)
				VALUES (?, ?, ?, ?, '', NULL, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)`,
				t.ID, p.ID, t.Name, t.Title, t.Class, state, defaultBranch, t.Branch, taskCapsJSON(t.Caps), taskTier(t.Tier), taskTier(t.Tier), t.Effort, t.Effort, fmtTime(p.Created), fmtTime(p.Updated))
		}
		if err != nil {
			return "", "", fmt.Errorf("rebuild task %d: %w", t.ID, err)
		}
	}

	if _, err := tx.Exec("DELETE FROM task_deps WHERE plan_id = ?", p.ID); err != nil {
		return "", "", fmt.Errorf("clear the edges of plan %d: %w", p.ID, err)
	}
	for _, t := range p.Tasks {
		for _, d := range t.Deps {
			if _, err := tx.Exec("INSERT INTO task_deps (plan_id, task_id, depends_on_task_id, is_base) VALUES (?, ?, ?, ?)", p.ID, t.ID, ids[d], d == t.Base); err != nil {
				return "", "", fmt.Errorf("rebuild an edge of plan %d: %w", p.ID, err)
			}
		}
	}

	// An open question lives in the event log (§3): the file's is written
	// back as a task_needs_help event unless the log already ends in it.
	for _, t := range p.Tasks {
		if t.Question == "" {
			continue
		}
		var latest string
		err := tx.QueryRow(`SELECT COALESCE(json_extract(payload, '$.question'), '') FROM events
			WHERE task_id = ? AND kind IN (?, ?, ?) ORDER BY id DESC LIMIT 1`,
			t.ID, evTaskNeedsHelp, evTaskAnswered, evTaskRestarted).Scan(&latest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("read the open question of task %d: %w", t.ID, err)
		}
		if latest == t.Question {
			continue
		}
		if _, err := appendEventTx(tx, evTaskNeedsHelp, project, t.ID, 0, eventPayload{Question: t.Question}, now); err != nil {
			return "", "", err
		}
	}

	if exists {
		action, detail = PlanUpdated, "the file was newer than the snapshot's plan"
	} else {
		action, detail = PlanRebuilt, "absent from the store"
	}
	if _, err := appendEventTx(tx, evPlanRecovered, project, 0, 0, eventPayload{Plan: p.ID, Status: action, Count: int64(len(p.Tasks))}, now); err != nil {
		return "", "", err
	}
	return action, strings.TrimSpace(detail), nil
}
