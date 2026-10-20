package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// statsLiveWindow is how recently a runner must have registered for its
// agents to count as live in the stats (§8). The engine keeps no other
// stale window: a runner's last_seen_at is written when it registers
// (§5.1), so this is the one judgment of "recently seen" there is.
const statsLiveWindow = 10 * time.Minute

// StatsRow is one (agent, class) line of a project's stats (§8): which
// agent, on which runner, how often it took the class in this repo and
// how often that succeeded, with the score the dispatcher ranks by.
// Attempts is also the confidence in the score: the more attempts, the
// less the prior pulls. An agent that offers the class and has no
// outcome row yet carries attempts 0 and the bare prior score, so a
// cold start shows instead of staying blank.
type StatsRow struct {
	AgentID            int64    `json:"agent_id"`
	Harness            string   `json:"harness"`
	Model              string   `json:"model"`
	Tier               string   `json:"tier"`
	Effort             string   `json:"effort,omitempty"` // the agent's effort level; outcomes still group by harness/model
	Account            string   `json:"account"`
	Runner             string   `json:"runner"`
	Live               bool     `json:"live"` // the runner was seen within the stats window
	Class              string   `json:"class"`
	Attempts           int      `json:"attempts"`
	Successes          int      `json:"successes"`
	Score              float64  `json:"score"`
	InputTokens        *int64   `json:"input_tokens"`
	OutputTokens       *int64   `json:"output_tokens"`
	CachedInputTokens  *int64   `json:"cached_input_tokens"`
	ReasoningTokens    *int64   `json:"reasoning_tokens"`
	Tokens             *int64   `json:"tokens"`
	CostUSD            *float64 `json:"cost_usd"`
	WallSeconds        *float64 `json:"wall_seconds"`
	ModelCalls         *int64   `json:"model_calls"`
	CallsPerJob        *float64 `json:"calls_per_job"`
	CallsPerSuccess    *float64 `json:"calls_per_success"`
	CostPerSuccess     *float64 `json:"cost_per_success"`
	CostPerSuccessUnit string   `json:"cost_per_success_unit,omitempty"`
}

// ProjectStats is one project's stats rows (§8), ordered by class and
// then best score first: the agent that wins a class leads it.
type ProjectStats struct {
	ProjectID int64      `json:"project_id"`
	RepoURL   string     `json:"repo_url"`
	Rows      []StatsRow `json:"rows"`
}

// Score is the success rate the dispatcher ranks agents by (§8): a rate
// with a prior of one pseudo-trial at 50%, (successes + 0.5) /
// (attempts + 1). With no attempts it is the prior itself, 0.5; each
// attempt moves it toward the observed rate, so a lone success reads
// 0.75, not 1.
func Score(attempts, successes int) float64 {
	return (float64(successes) + 0.5) / (float64(attempts) + 1)
}

// Stats reads what the outcome record has learned (§8): per project,
// every outcome row joined with its agent and the agent's runner, and
// every registered agent's offered class with no outcome row listed at
// attempts 0. project scopes the answer to one project; 0 names every
// project. A read only: it changes nothing and does not sweep.
func (e *Engine) Stats(ctx context.Context, project int64) ([]ProjectStats, error) {
	out := []ProjectStats{}
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = statsTx(tx, e.cfg, project, e.cfg.Now())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read stats: %w", err)
	}
	return out, nil
}

// statsAgent is one agent as the stats join sees it: the agent's row
// and its runner's liveness.
type statsAgent struct {
	StatsRow
	classes []string
}

func statsTx(tx *sql.Tx, cfg Config, project int64, now time.Time) ([]ProjectStats, error) {
	rows, err := tx.Query(`SELECT a.id, a.harness, a.model, a.effort, a.account, a.classes, r.name, COALESCE(r.last_seen_at, '')
		FROM agents a JOIN runners r ON r.id = a.runner_id ORDER BY a.id`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	agents := map[int64]*statsAgent{}
	var order []int64
	for rows.Next() {
		var a statsAgent
		var classes, seen string
		if err := rows.Scan(&a.AgentID, &a.Harness, &a.Model, &a.Effort, &a.Account, &classes, &a.Runner, &seen); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list agents: %w", err)
		}
		a.Tier = cfg.Caps.TierOf(a.Harness, a.Model)
		if err := json.Unmarshal([]byte(classes), &a.classes); err != nil {
			rows.Close()
			return nil, fmt.Errorf("agent %d: decode classes: %w", a.AgentID, err)
		}
		if t, err := parseTime(seen); err == nil {
			a.Live = now.Sub(t) <= statsLiveWindow
		}
		agents[a.AgentID] = &a
		order = append(order, a.AgentID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list agents: %w", err)
	}
	rows.Close()

	pquery := "SELECT id, repo_url FROM projects"
	pargs := []any{}
	if project != 0 {
		pquery += " WHERE id = ?"
		pargs = append(pargs, project)
	}
	pquery += " ORDER BY id"
	prows, err := tx.Query(pquery, pargs...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	var projects []ProjectStats
	for prows.Next() {
		var p ProjectStats
		if err := prows.Scan(&p.ProjectID, &p.RepoURL); err != nil {
			prows.Close()
			return nil, fmt.Errorf("list projects: %w", err)
		}
		p.Rows = []StatsRow{}
		projects = append(projects, p)
	}
	if err := prows.Err(); err != nil {
		prows.Close()
		return nil, fmt.Errorf("list projects: %w", err)
	}
	prows.Close()

	for i := range projects {
		p := &projects[i]
		type key struct {
			agent int64
			class string
		}
		seen := map[key]bool{}
		orows, err := tx.Query(`SELECT agent_id, class, attempts, successes FROM outcomes
			WHERE project_id = ? ORDER BY agent_id, class`, p.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("list outcomes of project %d: %w", p.ProjectID, err)
		}
		for orows.Next() {
			var agent int64
			var row StatsRow
			if err := orows.Scan(&agent, &row.Class, &row.Attempts, &row.Successes); err != nil {
				orows.Close()
				return nil, fmt.Errorf("list outcomes of project %d: %w", p.ProjectID, err)
			}
			a, ok := agents[agent]
			if !ok {
				continue // the agents row is gone; the cascade removes the outcome with it
			}
			attempts, successes, class := row.Attempts, row.Successes, row.Class
			row = a.StatsRow
			row.Class, row.Attempts, row.Successes = class, attempts, successes
			row.Score = Score(attempts, successes)
			p.Rows = append(p.Rows, row)
			seen[key{agent, class}] = true
		}
		if err := orows.Err(); err != nil {
			orows.Close()
			return nil, fmt.Errorf("list outcomes of project %d: %w", p.ProjectID, err)
		}
		orows.Close()

		// Offered but unrecorded: the cold start, visible at attempts 0.
		for _, id := range order {
			a := agents[id]
			for _, class := range a.classes {
				if seen[key{id, class}] {
					continue
				}
				seen[key{id, class}] = true
				row := a.StatsRow
				row.Class = class
				row.Score = Score(0, 0)
				p.Rows = append(p.Rows, row)
			}
		}
		if err := usageStatsTx(tx, p, agents); err != nil {
			return nil, err
		}
		sort.SliceStable(p.Rows, func(x, y int) bool {
			a, b := p.Rows[x], p.Rows[y]
			if a.Class != b.Class {
				return a.Class < b.Class
			}
			if a.Score != b.Score {
				return a.Score > b.Score
			}
			if a.Attempts != b.Attempts {
				return a.Attempts > b.Attempts
			}
			return a.AgentID < b.AgentID
		})
	}
	if projects == nil {
		projects = []ProjectStats{}
	}
	return projects, nil
}

// Usage is summed from jobs, not outcomes: failed runs, fix rounds and
// reviewers each spend their own agent's resources. SUM preserves NULL
// when none of the jobs reported a counter.
func usageStatsTx(tx *sql.Tx, p *ProjectStats, agents map[int64]*statsAgent) error {
	rows, err := tx.Query(`SELECT j.agent_id,
  CASE j.kind WHEN 'review' THEN 'review' WHEN 'foreman' THEN 'plan' ELSE COALESCE(t.class, 'implement') END AS class,
  SUM(j.input_tokens), SUM(j.output_tokens), SUM(j.cached_input_tokens), SUM(j.reasoning_tokens),
  SUM(CASE WHEN j.input_tokens IS NOT NULL OR j.output_tokens IS NOT NULL THEN COALESCE(j.input_tokens,0) + COALESCE(j.output_tokens,0) END),
  SUM(j.cost_usd), SUM(j.wall_seconds), SUM(j.model_calls), COUNT(j.model_calls)
  FROM jobs j LEFT JOIN tasks t ON t.id = j.task_id
  WHERE j.project_id = ? AND j.agent_id IS NOT NULL
  GROUP BY 1, 2`, p.ProjectID)
	if err != nil {
		return fmt.Errorf("sum job usage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u StatsRow
		var callJobs int64
		if err := rows.Scan(&u.AgentID, &u.Class, &u.InputTokens, &u.OutputTokens, &u.CachedInputTokens, &u.ReasoningTokens, &u.Tokens, &u.CostUSD, &u.WallSeconds, &u.ModelCalls, &callJobs); err != nil {
			return err
		}
		a := agents[u.AgentID]
		if a == nil {
			continue
		}
		index := -1
		for i := range p.Rows {
			if p.Rows[i].AgentID == u.AgentID && p.Rows[i].Class == u.Class {
				index = i
				break
			}
		}
		if index < 0 {
			row := a.StatsRow
			row.Class = u.Class
			row.Score = Score(0, 0)
			p.Rows = append(p.Rows, row)
			index = len(p.Rows) - 1
		}
		row := &p.Rows[index]
		row.InputTokens, row.OutputTokens, row.CachedInputTokens, row.ReasoningTokens = u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.ReasoningTokens
		row.Tokens, row.CostUSD, row.WallSeconds = u.Tokens, u.CostUSD, u.WallSeconds
		row.ModelCalls = u.ModelCalls
		if u.ModelCalls != nil {
			if callJobs > 0 {
				perJob := float64(*u.ModelCalls) / float64(callJobs)
				row.CallsPerJob = &perJob
			}
			if row.Successes > 0 {
				perSuccess := float64(*u.ModelCalls) / float64(row.Successes)
				row.CallsPerSuccess = &perSuccess
			}
		}
		if row.Successes > 0 {
			var cost float64
			if row.CostUSD != nil {
				cost = *row.CostUSD
				row.CostPerSuccessUnit = "USD"
			} else if row.Tokens != nil {
				cost = float64(*row.Tokens)
				row.CostPerSuccessUnit = "tokens"
			} else {
				continue
			}
			cost /= float64(row.Successes)
			row.CostPerSuccess = &cost
		}
	}
	return rows.Err()
}
