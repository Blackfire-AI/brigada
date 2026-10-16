package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// wakeDayBounds returns the UTC day window the wake budget counts in
// (§4.3): its start — UTC midnight, the moment the count resets — and
// its end, the reset time the event, the notification and the needs-you
// entry carry.
func wakeDayBounds(now time.Time) (start, end time.Time) {
	start = now.Truncate(24 * time.Hour)
	return start, start.Add(24 * time.Hour)
}

// foremanWakeBudgetTx reads the project's effective daily foreman wake
// budget (§4.3): the foreman_wakes_per_day setting when the row holds a
// positive one, else the engine's configured default (the config file's
// key, else the built-in 20). set reports whether a setting stands; a
// value that does not parse positive is the default — the setting path
// validates, so only a hand-edited row lands there.
func (e *Engine) foremanWakeBudgetTx(tx *sql.Tx, project int64) (budget int, set bool, err error) {
	var raw string
	if err := tx.QueryRow("SELECT settings FROM projects WHERE id = ?", project).Scan(&raw); err != nil {
		return 0, false, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	if raw != "" {
		var present map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &present); err != nil {
			return 0, false, fmt.Errorf("parse the settings of project %d: %w", project, err)
		}
		if n, ok := rawInt(present[SettingForemanWakesPerDay]); ok && n > 0 {
			return n, true, nil
		}
	}
	return e.cfg.ForemanWakeBudget, false, nil
}

// WakeBudgetInfo is one project's foreman wake budget (§4.3) as
// `brigada budget` shows it: the effective daily budget, today's use
// since UTC midnight, the window's end and how many inbox rows wait.
type WakeBudgetInfo struct {
	ProjectID int64  `json:"project_id"`
	RepoURL   string `json:"repo_url"`
	Budget    int    `json:"budget"`
	Wakes     int    `json:"wakes"`
	ResetsAt  string `json:"resets_at"`
	Waiting   int    `json:"waiting"`
	// Default says no per-project setting stands: the engine's default
	// (the config file's foreman_wakes_per_day, else the built-in 20)
	// is the budget in effect.
	Default bool `json:"default"`
}

// wakeBudgetTx computes one project's WakeBudgetInfo at now.
func (e *Engine) wakeBudgetTx(tx *sql.Tx, project int64, repo string, now time.Time) (WakeBudgetInfo, error) {
	dayStart, dayEnd := wakeDayBounds(now)
	info := WakeBudgetInfo{ProjectID: project, RepoURL: repo, ResetsAt: fmtTime(dayEnd)}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE project_id = ? AND kind = ? AND created_at >= ?`,
		project, KindForeman, fmtTime(dayStart)).Scan(&info.Wakes); err != nil {
		return WakeBudgetInfo{}, fmt.Errorf("count the day's foreman turns of project %d: %w", project, err)
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM foreman_inbox WHERE project_id = ? AND state = ?`,
		project, inboxPending).Scan(&info.Waiting); err != nil {
		return WakeBudgetInfo{}, fmt.Errorf("count the pending events of project %d: %w", project, err)
	}
	budget, set, err := e.foremanWakeBudgetTx(tx, project)
	if err != nil {
		return WakeBudgetInfo{}, err
	}
	info.Budget, info.Default = budget, !set
	return info, nil
}

// WakeBudget reads one project's foreman wake budget (§4.3): the
// effective budget, today's use, the reset time and the waiting events.
// repoURL names the project; empty names the engine's only one, the way
// `brigada ask` resolves a project. ErrNoProject when none matches.
func (e *Engine) WakeBudget(ctx context.Context, repoURL string) (*WakeBudgetInfo, error) {
	var out *WakeBudgetInfo
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		project, err := askProjectTx(tx, repoURL)
		if err != nil {
			return err
		}
		var repo string
		if err := tx.QueryRow("SELECT repo_url FROM projects WHERE id = ?", project).Scan(&repo); err != nil {
			return fmt.Errorf("read the repo of project %d: %w", project, err)
		}
		info, err := e.wakeBudgetTx(tx, project, repo, now)
		if err != nil {
			return err
		}
		out = &info
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetWakeBudget sets the project's daily foreman wake budget (§4.3)
// through the settings path, then re-evaluates the waiting wake at
// once, in the raise's own transaction: events parked by the spent
// budget reach a turn now, not at the next judgment event. A budget
// lower than today's use parks the next wake instead. The project is
// found by URL — empty names the engine's only project — and born by
// the set when the repo has none.
func (e *Engine) SetWakeBudget(ctx context.Context, repoURL string, wakes int) (WakeBudgetInfo, error) {
	if wakes < 1 || wakes > MaxForemanWakesPerDay {
		return WakeBudgetInfo{}, fmt.Errorf("%w: %s takes a positive whole number of foreman wakes per day, at most %d, got %d",
			ErrInvalidSetting, SettingForemanWakesPerDay, MaxForemanWakesPerDay, wakes)
	}
	if repoURL == "" {
		// Empty names the engine's only project (the ask rule), and the
		// settings path needs the URL it stands for.
		err := e.store.View(ctx, func(tx *sql.Tx) error {
			project, err := askProjectTx(tx, "")
			if err != nil {
				return err
			}
			return tx.QueryRow("SELECT repo_url FROM projects WHERE id = ?", project).Scan(&repoURL)
		})
		if err != nil {
			return WakeBudgetInfo{}, err
		}
	}
	got, err := e.SetProjectSettings(ctx, repoURL, map[string]string{SettingForemanWakesPerDay: strconv.Itoa(wakes)})
	if err != nil {
		return WakeBudgetInfo{}, err
	}
	var out WakeBudgetInfo
	err = e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		if err := e.wakeForemanTx(tx, got.ProjectID, now); err != nil {
			return err
		}
		out, err = e.wakeBudgetTx(tx, got.ProjectID, got.RepoURL, now)
		return err
	})
	if err != nil {
		return WakeBudgetInfo{}, fmt.Errorf("set the wake budget of the project of %s: %w", RedactRepoURL(repoURL), err)
	}
	return out, nil
}

// statusWakeBudgetTx lists the projects whose wake the day's budget
// holds (§4.3, §10): pending inbox rows, the budget spent, and no user
// message among them — a wake that carries one is not charged, so it
// goes ahead and nothing waits on the budget. The entry disappears on
// its own: the moment nothing waits, or the budget is raised or the day
// resets, the query below finds no row to list.
func (e *Engine) statusWakeBudgetTx(tx *sql.Tx, project int64, now time.Time) ([]NeedsYouItem, error) {
	dayStart, dayEnd := wakeDayBounds(now)
	query := `SELECT i.project_id, p.repo_url, COUNT(*),
			SUM(CASE WHEN e.kind = ? THEN 1 ELSE 0 END)
		FROM foreman_inbox i
		JOIN events e ON e.id = i.event_id
		JOIN projects p ON p.id = i.project_id
		WHERE i.state = ?`
	args := []any{evUserMessage, inboxPending}
	if project != 0 {
		query += " AND i.project_id = ?"
		args = append(args, project)
	}
	query += " GROUP BY i.project_id, p.repo_url ORDER BY i.project_id"
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list the projects with waiting events: %w", err)
	}
	type waiting struct {
		project int64
		repo    string
		rows    int
	}
	var pend []waiting
	for rows.Next() {
		var w waiting
		var userMsgs int
		if err := rows.Scan(&w.project, &w.repo, &w.rows, &userMsgs); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the projects with waiting events: %w", err)
		}
		// A user message in the inbox takes the wake past the budget
		// (§4.3): the budget holds nothing.
		if userMsgs > 0 {
			continue
		}
		pend = append(pend, w)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list the projects with waiting events: %w", err)
	}
	rows.Close()
	items := []NeedsYouItem{}
	for _, w := range pend {
		var wakes int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs WHERE project_id = ? AND kind = ? AND created_at >= ?`,
			w.project, KindForeman, fmtTime(dayStart)).Scan(&wakes); err != nil {
			return nil, fmt.Errorf("count the day's foreman turns of project %d: %w", w.project, err)
		}
		budget, _, err := e.foremanWakeBudgetTx(tx, w.project)
		if err != nil {
			return nil, err
		}
		if wakes < budget {
			continue
		}
		items = append(items, NeedsYouItem{
			Kind:  "foreman",
			ID:    w.project,
			State: "budget_spent",
			Title: w.repo,
			Reason: fmt.Sprintf("wake budget spent (%d/%d today, resets %s); %d events waiting — raise it with `brigada budget --wakes N` or send `brigada ask`",
				wakes, budget, fmtTime(dayEnd), w.rows),
		})
	}
	return items, nil
}
