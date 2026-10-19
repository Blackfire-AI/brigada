// Package caps is the concurrency-caps model of design §8.1: which
// layers a job is counted against, what those layers hold now, and
// whether a job still fits. The model is pure — it reads no store; the
// engine builds a Usage from the live jobs and asks Room about each
// dispatch it considers.
package caps

import "github.com/Blackfire-AI/brigada/internal/adapter"

// Limits are the configured caps (§8.1): a job may start only if every
// layer on its path has room. A nil pointer, or a missing map entry,
// means the layer is not capped; a set limit of 0 caps the layer at
// nothing, which is different from not capping it.
//
// Cost is not a limit but is configured beside them: the relative cost
// weight of one (harness, model) pair, which the foreman reads through
// `capacity` (§12.2). A missing entry there is the default weight of 1.
// Tier overrides the model-name table for every agent of a (harness, model)
// pair. Outcome record keys stay (repo, class, agent), so records earned
// under one label stay with the agent when an override changes its tier.
type Limits struct {
	Global  *int              // the engine's overall ceiling
	Plan    *int              // max parallel tasks per plan
	Runner  *int              // the config's runner cap; a runner may advertise its own
	Account map[string]int    // by account label
	Model   map[string]int    // by model; a per-model cap such as 1 for a heavy model
	Cost    map[string]int    // by "harness/model"
	Tier    map[string]string // by "harness/model"

	// NoHeadroom turns the foreman headroom rule (§8.2) off. The rule
	// is on by default, so the zero value keeps it.
	NoHeadroom bool
}

// HeadroomCap is the account cap a task job sees on the foreman's
// account (§8.2): one lower than the configured cap, never below 0, so
// one slot stays reserved for the foreman. An uncapped account (nil
// limit) stays uncapped: there is no cap to lower.
func HeadroomCap(limit int) int {
	return max(limit-1, 0)
}

// CostOf is the cost weight of one (harness, model) pair (§8.1): what
// the config named, else the default of 1.
func (l Limits) CostOf(harness, model string) int {
	if n, ok := l.Cost[harness+"/"+model]; ok {
		return n
	}
	return 1
}

// TierOf is the configured tier of a (harness, model) pair, else its table tier.
func (l Limits) TierOf(harness, model string) string {
	if tier, ok := l.Tier[harness+"/"+model]; ok {
		return tier
	}
	return adapter.AgentTier(harness, model)
}

// RunnerCap folds the config's runner cap with the cap the runner
// advertised when it registered (§8.1): the smaller of the two,
// ignoring whichever is unset. advertised follows the runners table:
// 0 means the runner advertised none.
func (l Limits) RunnerCap(advertised int) *int {
	switch {
	case l.Runner == nil && advertised <= 0:
		return nil
	case l.Runner == nil:
		return &advertised
	case advertised <= 0:
		return l.Runner
	case *l.Runner <= advertised:
		return l.Runner
	}
	return &advertised
}

// Job is the dispatch Room is asked about: which runner would take the
// job, through which account and model, for which plan, and whether it
// is a foreman turn. Plan is 0 for a foreman turn.
type Job struct {
	Runner    int64
	RunnerCap int    // the runner's advertised max_parallel; 0 = unset
	Account   string // the agent's account label; "" = no agent recorded
	Model     string
	Plan      int64
	Foreman   bool
	// Headroom marks a task job whose account is the foreman's (§8.2):
	// the account layer sees HeadroomCap of the configured cap.
	Headroom bool
}

// Usage is the live-job count per layer (§8.1). Live means leased or
// running: a queued job holds no runner, no agent and no slot. An empty
// account or model — no agent recorded on the job — and a plan of 0 — a
// foreman turn — count at the layers that have them.
type Usage struct {
	Runner  map[int64]int
	Account map[string]int
	Model   map[string]int
	Plan    map[int64]int
	Global  int
}

// Add counts one live job into the usage.
func (u *Usage) Add(runner int64, account, model string, plan int64) {
	if u.Runner == nil {
		u.Runner = map[int64]int{}
	}
	if u.Account == nil {
		u.Account = map[string]int{}
	}
	if u.Model == nil {
		u.Model = map[string]int{}
	}
	if u.Plan == nil {
		u.Plan = map[int64]int{}
	}
	u.Runner[runner]++
	if account != "" {
		u.Account[account]++
	}
	if model != "" {
		u.Model[model]++
	}
	if plan != 0 {
		u.Plan[plan]++
	}
	u.Global++
}

// The layers, in the order Room reports them full (§8.1's order).
const (
	LayerRunner  = "runner"
	LayerAccount = "account"
	LayerModel   = "model"
	LayerPlan    = "plan"
	LayerGlobal  = "global"
)

// Room reports whether every capped layer has room for job (§8.1), and
// names the first one that does not: runner, account, model, plan,
// global. A foreman turn counts toward the runner, account and global
// caps only — the plan and model layers ignore it — so the foreman is
// never starved by its own tasks.
func Room(limits Limits, usage Usage, job Job) (bool, string) {
	if limit := limits.RunnerCap(job.RunnerCap); limit != nil && usage.Runner[job.Runner] >= *limit {
		return false, LayerRunner
	}
	if limit, capped := limits.Account[job.Account]; capped {
		if job.Headroom && !job.Foreman && !limits.NoHeadroom {
			limit = HeadroomCap(limit)
		}
		if usage.Account[job.Account] >= limit {
			return false, LayerAccount
		}
	}
	if !job.Foreman {
		if limit, capped := limits.Model[job.Model]; capped && usage.Model[job.Model] >= limit {
			return false, LayerModel
		}
		if limits.Plan != nil && usage.Plan[job.Plan] >= *limits.Plan {
			return false, LayerPlan
		}
	}
	if limits.Global != nil && usage.Global >= *limits.Global {
		return false, LayerGlobal
	}
	return true, ""
}
