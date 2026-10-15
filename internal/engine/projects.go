package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// ForemanSetting is a project's foreman harness and model (§4.1, §11):
// the user's choice of which CLI runs the foreman and which model it
// runs on. The harness must resume headless — a foreman turn wakes and
// continues the project's session without a terminal (§4.2).
type ForemanSetting struct {
	ProjectID int64  `json:"project_id"`
	RepoURL   string `json:"repo_url"`
	Harness   string `json:"harness"`
	Model     string `json:"model"`
}

// ErrInvalidForeman marks a refusal the caller caused: a foreman setting
// the engine will not take, whatever its reason — an empty field, a
// harness that cannot resume headless. The API turns it into the
// caller's 400; every other SetProjectForeman error is the engine's own
// and surfaces as an internal failure.
var ErrInvalidForeman = errors.New("invalid foreman setting")

// setupDefaultBranch is the default branch a project born by setup
// takes: the foreman choice has no base ref to derive one from (§4.1),
// so it takes the conventional 'main', where the debug task submit
// reads the branch off the base ref.
const setupDefaultBranch = "main"

// SetProjectForeman records the user's choice of the foreman's harness
// and model for the repo (§4.1). The harness must come from the
// resume-capable set — a foreman turn wakes headless — and neither the
// harness nor the model may be empty. The project is found by URL or
// created, born by setup with setupDefaultBranch. The setting lands
// with its event in one transaction, so the log never names a choice
// the row does not hold.
func (e *Engine) SetProjectForeman(ctx context.Context, repoURL, harness, model string) (ForemanSetting, error) {
	if repoURL == "" {
		return ForemanSetting{}, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidForeman)
	}
	if harness == "" {
		return ForemanSetting{}, fmt.Errorf("%w: foreman harness is required: the CLI the foreman runs on (§4.1)", ErrInvalidForeman)
	}
	if model == "" {
		return ForemanSetting{}, fmt.Errorf("%w: foreman model is required: the model the foreman runs on (§4.1)", ErrInvalidForeman)
	}
	if !adapter.ResumeCapable(harness) {
		return ForemanSetting{}, fmt.Errorf("%w: harness %q cannot resume headless, which the foreman needs to wake on its session (§4.1); choose a resume-capable harness: %s",
			ErrInvalidForeman, harness, strings.Join(resumeCapableHarnesses(), ", "))
	}

	setting := ForemanSetting{RepoURL: repoURL, Harness: harness, Model: model}
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		var oldHarness, oldModel string
		cleared := false
		err := tx.QueryRow("SELECT id, foreman_harness, foreman_model FROM projects WHERE repo_url = ?", repoURL).
			Scan(&project, &oldHarness, &oldModel)
		switch {
		case err == nil:
			if _, err := tx.Exec("UPDATE projects SET foreman_harness = ?, foreman_model = ?, updated_at = ? WHERE id = ?",
				harness, model, fmtTime(now), project); err != nil {
				return fmt.Errorf("set the foreman of the project of %s: %w", repoURL, err)
			}
			if harness != oldHarness || model != oldModel {
				// The user chose to switch (§4.4): the limit was the old
				// foreman's, so the new one starts unlimited.
				res, err := tx.Exec("UPDATE projects SET foreman_limited = NULL WHERE id = ? AND foreman_limited IS NOT NULL", project)
				if err != nil {
					return fmt.Errorf("clear the foreman limit of the project of %s: %w", repoURL, err)
				}
				n, _ := res.RowsAffected()
				cleared = n > 0
			}
			if harness != oldHarness {
				// A session id belongs to one harness's CLI: the new
				// foreman starts fresh, rebuilding its context from the
				// engine, and never resumes the old harness's session.
				if _, err := tx.Exec("UPDATE sessions SET harness_session_id = NULL WHERE key = ?",
					fmt.Sprintf("foreman:%d", project)); err != nil {
					return fmt.Errorf("drop the old foreman session of the project of %s: %w", repoURL, err)
				}
			}
		case errors.Is(err, sql.ErrNoRows):
			project, err = createProjectTx(tx, repoURL, setupDefaultBranch, harness, model, now)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("read the project of %s: %w", repoURL, err)
		}
		if _, err := appendEventTx(tx, evProjectForemanSet, project, 0, 0,
			eventPayload{Harness: harness, Model: model}, now); err != nil {
			return err
		}
		setting.ProjectID = project
		if cleared {
			// The events the limit held back reach the new foreman now,
			// not at the next event.
			return e.wakeForemanTx(tx, project, now)
		}
		return nil
	})
	if err != nil {
		return ForemanSetting{}, fmt.Errorf("set the foreman of the project of %s: %w", repoURL, err)
	}
	return setting, nil
}

// ProjectForeman reads the foreman's harness and model back (§4.1); nil
// when the repo has no project. Like every engine call it sweeps first,
// so the answer is the rules' current word, not a row as it happens to
// sit.
func (e *Engine) ProjectForeman(ctx context.Context, repoURL string) (*ForemanSetting, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("repo URL is required (product §5 rule 6: repos are addressed by URL)")
	}
	var setting *ForemanSetting
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := e.sweepTx(tx, e.cfg.Now()); err != nil {
			return err
		}
		var f ForemanSetting
		var project int64
		err := tx.QueryRow("SELECT id, foreman_harness, foreman_model FROM projects WHERE repo_url = ?",
			repoURL).Scan(&project, &f.Harness, &f.Model)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the project of %s: %w", repoURL, err)
		}
		f.ProjectID, f.RepoURL = project, repoURL
		setting = &f
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the foreman of the project of %s: %w", repoURL, err)
	}
	return setting, nil
}

// createProjectTx inserts one project row (§4.1, §11): the repo URL, its
// default branch and the foreman's harness and model, empty until setup
// chooses them. SubmitTask and SetProjectForeman share the insert; only
// the default branch's derivation differs — the task submit reads it off
// the base ref, setup has no ref and takes setupDefaultBranch.
func createProjectTx(tx *sql.Tx, repoURL, defaultBranch, harness, model string, now time.Time) (int64, error) {
	nowStr := fmtTime(now)
	res, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES (?, ?, '{}', ?, ?, ?, ?)`, repoURL, defaultBranch, harness, model, nowStr, nowStr)
	if err != nil {
		return 0, fmt.Errorf("create the project of %s: %w", repoURL, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create the project of %s: last insert id: %w", repoURL, err)
	}
	return id, nil
}

// resumeCapableHarnesses lists the harnesses a foreman may run on, for
// the refusal that names them; sorted, so the message is deterministic.
func resumeCapableHarnesses() []string {
	names := make([]string, 0, len(adapter.ResumeCapableHarnesses))
	for name := range adapter.ResumeCapableHarnesses {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
