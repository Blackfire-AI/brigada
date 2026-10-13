package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// CapacitySnapshot is the foreman's capacity view (§4.5, §12.2): a
// read-only copy of what the dispatcher would see right now. Agents
// carries one entry per advertised agent — its effective cap and the
// layer that sets it, the jobs running on it, its quota state and its
// cost weight. Layers carries one entry per cap layer — every runner,
// every account, every capped model, every active plan of the project,
// and global — with the layer's cap, its use and its queue depth. The
// percent-of-window quota figures §12.2 names ride on CodexBar data
// and are not part of the snapshot yet.
type CapacitySnapshot struct {
	Agents []CapacityAgent `json:"agents"`
	Layers []CapacityLayer `json:"layers"`
}

// CapacityAgent is one advertised agent as capacity reports it
// (§12.2).
type CapacityAgent struct {
	ID       int64  `json:"id"`
	RunnerID int64  `json:"runner_id"`
	Harness  string `json:"harness"`
	Model    string `json:"model"`
	Account  string `json:"account"`
	// Cap is the effective cap — the smallest of the runner, account
	// and model caps on the agent's path — and CapLayer names the
	// layer that sets it (the caps package's layer names, or "none").
	// A nil Cap means no layer caps the agent; a Cap of 0 means capped
	// at nothing, which is different from not capped (§8.1).
	Cap      *int   `json:"cap"`
	CapLayer string `json:"cap_layer"`
	// Running counts the jobs on the agent — leased or running, the
	// ones that recorded it. A queued job holds no agent and never
	// counts (§12.2).
	Running int `json:"running"`
	// Quota is the agent's quota state (§8): ready, cooling — with
	// CoolingUntil, the store's UTC text — or broken. Broken wins over
	// cooling: a broken agent takes nothing whatever its cooling says.
	Quota        string `json:"quota"`
	CoolingUntil string `json:"cooling_until,omitempty"`
	// Cost is the agent's cost weight (§8.1): the configured weight of
	// its (harness, model) pair, 1 when the config names none.
	Cost int `json:"cost"`
}

// CapacityLayer is one cap layer's row as capacity reports it (§12.2):
// exactly one of RunnerID, Account, Model and PlanID names the
// instance, and all stay zero — empty — for the global layer.
type CapacityLayer struct {
	Layer string `json:"layer"` // the caps package's layer names: runner, account, model, plan, global
	// RunnerID names a runner layer's instance, Account an account's,
	// Model a capped model's and PlanID an active plan's.
	RunnerID int64  `json:"runner_id,omitempty"`
	Account  string `json:"account,omitempty"`
	Model    string `json:"model,omitempty"`
	PlanID   int64  `json:"plan_id,omitempty"`
	// Cap is the instance's cap; nil means the layer is not capped. A
	// runner entry carries the folded cap — the smaller of the
	// config's and the one the runner advertised (§8.1).
	Cap *int `json:"cap"`
	// Use counts the live jobs through the instance; Queue counts the
	// queued jobs the instance holds (§12.2).
	Use   int `json:"use"`
	Queue int `json:"queue"`
}

// The quota states an agent's snapshot entry carries (§8), and the
// cap-layer name that says no layer caps the agent at all.
const (
	quotaReady   = "ready"
	quotaCooling = "cooling"
	quotaBroken  = "broken"

	capLayerNone = "none"
)

// Capacity reads the capacity snapshot (§12.2) in one read-only
// transaction: a copy of what the dispatcher would see, never a state
// change. projectID limits the plan layer to that project's active
// plans — the foreman's own project is what its token scopes it to
// (§12.1) — while 0 lists every active plan, for a caller that is not
// one project's foreman.
func (e *Engine) Capacity(ctx context.Context, projectID int64) (CapacitySnapshot, error) {
	var snap CapacitySnapshot
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		var err error
		snap, err = capacityTx(tx, e.cfg, projectID, e.cfg.Now())
		return err
	})
	if err != nil {
		return CapacitySnapshot{}, fmt.Errorf("read capacity: %w", err)
	}
	return snap, nil
}

// capacityAgent is one agents row as the snapshot reads it.
type capacityAgent struct {
	id, runner      int64
	harness, model  string
	account, health string
	coolingUntil    string
}

// capacityTx builds the capacity snapshot (§12.2) inside the caller's
// transaction. The layers' use counts come from the caps model's own
// usage view (§8.1); their queue depths come from the cap-hold stamps
// the lease gate leaves on queued jobs (§8.1).
func capacityTx(tx *sql.Tx, cfg Config, projectID int64, now time.Time) (CapacitySnapshot, error) {
	usage, err := capsUsageTx(tx)
	if err != nil {
		return CapacitySnapshot{}, err
	}

	// The runner rows: each runner gets a layer entry with the folded
	// cap, and each agent's effective cap starts from its runner's.
	runnerCaps := map[int64]*int{}
	rows, err := tx.Query("SELECT id, max_parallel FROM runners ORDER BY id")
	if err != nil {
		return CapacitySnapshot{}, fmt.Errorf("list runners: %w", err)
	}
	for rows.Next() {
		var id int64
		var advertised int
		if err := rows.Scan(&id, &advertised); err != nil {
			rows.Close()
			return CapacitySnapshot{}, fmt.Errorf("list runners: %w", err)
		}
		runnerCaps[id] = cfg.Caps.RunnerCap(advertised)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CapacitySnapshot{}, fmt.Errorf("list runners: %w", err)
	}
	rows.Close()

	agents := []capacityAgent{}
	rows, err = tx.Query(`SELECT id, runner_id, harness, model, account, health, COALESCE(cooling_until, '')
		FROM agents ORDER BY id`)
	if err != nil {
		return CapacitySnapshot{}, fmt.Errorf("list agents: %w", err)
	}
	for rows.Next() {
		var a capacityAgent
		if err := rows.Scan(&a.id, &a.runner, &a.harness, &a.model, &a.account, &a.health, &a.coolingUntil); err != nil {
			rows.Close()
			return CapacitySnapshot{}, fmt.Errorf("list agents: %w", err)
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CapacitySnapshot{}, fmt.Errorf("list agents: %w", err)
	}
	rows.Close()

	// The live jobs each agent carries: leased or running, the ones
	// that recorded an agent when they took their lease.
	running := map[int64]int{}
	rows, err = tx.Query(`SELECT agent_id, COUNT(*) FROM jobs
		WHERE status IN ('leased', 'running') AND agent_id IS NOT NULL GROUP BY agent_id`)
	if err != nil {
		return CapacitySnapshot{}, fmt.Errorf("count jobs per agent: %w", err)
	}
	for rows.Next() {
		var agent, n int64
		if err := rows.Scan(&agent, &n); err != nil {
			rows.Close()
			return CapacitySnapshot{}, fmt.Errorf("count jobs per agent: %w", err)
		}
		running[agent] = int(n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CapacitySnapshot{}, fmt.Errorf("count jobs per agent: %w", err)
	}
	rows.Close()

	// The queued jobs a layer holds (§8.1), attributed to their layer
	// instances by the cap_held_key stamp the lease gate wrote when the
	// hold happened (§12.2) — the polling runner's id, the account label,
	// the model name or the plan id. The stamp is exact: the snapshot
	// never guesses the instance from the harness, so a hold credits the
	// runner, account or model that actually had no room. A plan hold
	// stamped before the key column existed falls back to the plan its
	// task names; a hold with no instance to name — global — counts at
	// the global layer alone.
	type hold struct {
		layer, key string
		plan       int64 // the task's plan: the fallback for a plan hold without a key
	}
	held, err := tx.Query(`SELECT COALESCE(j.cap_held_layer, ''), COALESCE(j.cap_held_key, ''), COALESCE(t.plan_id, 0)
		FROM jobs j LEFT JOIN tasks t ON t.id = j.task_id
		WHERE j.status = 'queued' AND j.cap_held_layer IS NOT NULL ORDER BY j.id`)
	if err != nil {
		return CapacitySnapshot{}, fmt.Errorf("list held jobs: %w", err)
	}
	var holds []hold
	for held.Next() {
		var h hold
		if err := held.Scan(&h.layer, &h.key, &h.plan); err != nil {
			held.Close()
			return CapacitySnapshot{}, fmt.Errorf("list held jobs: %w", err)
		}
		holds = append(holds, h)
	}
	if err := held.Err(); err != nil {
		held.Close()
		return CapacitySnapshot{}, fmt.Errorf("list held jobs: %w", err)
	}
	held.Close()
	queueRunner := map[int64]int{}
	queueAccount := map[string]int{}
	queueModel := map[string]int{}
	queuePlan := map[int64]int{}
	queueGlobal := 0
	for _, h := range holds {
		switch h.layer {
		case caps.LayerRunner:
			id, err := strconv.ParseInt(h.key, 10, 64)
			if err != nil {
				continue // a hold without its instance names nobody
			}
			queueRunner[id]++
		case caps.LayerAccount:
			queueAccount[h.key]++
		case caps.LayerModel:
			queueModel[h.key]++
		case caps.LayerPlan:
			plan, err := strconv.ParseInt(h.key, 10, 64)
			if err != nil {
				plan = h.plan
			}
			if plan != 0 {
				queuePlan[plan]++
			}
		case caps.LayerGlobal:
			queueGlobal++
		}
	}

	snap := CapacitySnapshot{Agents: []CapacityAgent{}, Layers: []CapacityLayer{}}
	nowStr := fmtTime(now)

	// Agents, in id order: the effective cap and its layer, then the
	// quota state and the cost weight.
	for _, a := range agents {
		cap, layer := effectiveCap(cfg.Caps, runnerCaps[a.runner], a.account, a.model)
		quota, coolingUntil := quotaState(a.health, a.coolingUntil, nowStr)
		snap.Agents = append(snap.Agents, CapacityAgent{
			ID:           a.id,
			RunnerID:     a.runner,
			Harness:      a.harness,
			Model:        a.model,
			Account:      a.account,
			Cap:          cap,
			CapLayer:     layer,
			Running:      running[a.id],
			Quota:        quota,
			CoolingUntil: coolingUntil,
			Cost:         cfg.Caps.CostOf(a.harness, a.model),
		})
	}

	// The runner layer: one entry per registered runner, in id order.
	runnerIDs := make([]int64, 0, len(runnerCaps))
	for id := range runnerCaps {
		runnerIDs = append(runnerIDs, id)
	}
	sort.Slice(runnerIDs, func(i, j int) bool { return runnerIDs[i] < runnerIDs[j] })
	for _, id := range runnerIDs {
		snap.Layers = append(snap.Layers, CapacityLayer{
			Layer: caps.LayerRunner, RunnerID: id,
			Cap: runnerCaps[id], Use: usage.Runner[id], Queue: queueRunner[id],
		})
	}

	// The account layer: one entry per account in play — every account
	// an agent advertises, plus every account the config caps, so a
	// configured cap on an idle account still shows.
	accountSet := map[string]bool{}
	for _, a := range agents {
		accountSet[a.account] = true
	}
	for account := range cfg.Caps.Account {
		accountSet[account] = true
	}
	accounts := make([]string, 0, len(accountSet))
	for account := range accountSet {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	for _, account := range accounts {
		snap.Layers = append(snap.Layers, CapacityLayer{
			Layer: caps.LayerAccount, Account: account,
			Cap: accountCap(cfg.Caps, account), Use: usage.Account[account], Queue: queueAccount[account],
		})
	}

	// The model layer: one entry per capped model (§12.2), sorted so
	// the snapshot's order stays deterministic whatever the config's
	// map order was.
	models := make([]string, 0, len(cfg.Caps.Model))
	for model := range cfg.Caps.Model {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		snap.Layers = append(snap.Layers, CapacityLayer{
			Layer: caps.LayerModel, Model: model,
			Cap: modelCap(cfg.Caps, model), Use: usage.Model[model], Queue: queueModel[model],
		})
	}

	// The plan layer: one entry per active plan of the project — 0
	// means every project's. A draft or finished plan holds no tasks
	// and earns no entry.
	planQuery := "SELECT id FROM plans WHERE state = 'active'"
	var args []any
	if projectID != 0 {
		planQuery += " AND project_id = ?"
		args = append(args, projectID)
	}
	planQuery += " ORDER BY id"
	rows, err = tx.Query(planQuery, args...)
	if err != nil {
		return CapacitySnapshot{}, fmt.Errorf("list active plans: %w", err)
	}
	var planIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return CapacitySnapshot{}, fmt.Errorf("list active plans: %w", err)
		}
		planIDs = append(planIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CapacitySnapshot{}, fmt.Errorf("list active plans: %w", err)
	}
	rows.Close()
	for _, id := range planIDs {
		snap.Layers = append(snap.Layers, CapacityLayer{
			Layer: caps.LayerPlan, PlanID: id,
			Cap: cfg.Caps.Plan, Use: usage.Plan[id], Queue: queuePlan[id],
		})
	}

	// The global layer, last like in Room's report order (§8.1).
	snap.Layers = append(snap.Layers, CapacityLayer{
		Layer: caps.LayerGlobal,
		Cap:   cfg.Caps.Global, Use: usage.Global, Queue: queueGlobal,
	})
	return snap, nil
}

// quotaState reads an agent's quota state (§8): broken wins over
// cooling — a broken agent takes nothing whatever its cooling says —
// and a cooling deadline at or before now has lapsed, so the agent is
// ready. The timestamps are the store's fixed-width UTC text, so the
// comparison is lexicographic.
func quotaState(health, coolingUntil, nowStr string) (quota, until string) {
	if health == quotaBroken {
		return quotaBroken, ""
	}
	if coolingUntil > nowStr {
		return quotaCooling, coolingUntil
	}
	return quotaReady, ""
}

// effectiveCap is the smallest cap on an agent's path — its runner's
// folded cap, its account's and its model's (§8.1) — and the layer
// that sets it, "none" when no layer caps the agent. Ties keep the
// outermost layer, the runner before the account before the model,
// the order Room reports layers in. A nil cap with "none" means
// uncapped; a cap of 0 means capped at nothing (§8.1).
func effectiveCap(limits caps.Limits, runnerCap *int, account, model string) (*int, string) {
	best, layer := runnerCap, caps.LayerRunner
	consider := func(v int, l string) {
		if best == nil || v < *best {
			best, layer = &v, l
		}
	}
	if v, ok := limits.Account[account]; ok {
		consider(v, caps.LayerAccount)
	}
	if v, ok := limits.Model[model]; ok {
		consider(v, caps.LayerModel)
	}
	if best == nil {
		return nil, capLayerNone
	}
	return best, layer
}

// accountCap is the configured cap of one account layer, nil when the
// config does not cap it at all.
func accountCap(limits caps.Limits, account string) *int {
	if v, ok := limits.Account[account]; ok {
		return &v
	}
	return nil
}

// modelCap is the configured cap of one model layer, nil when the
// config does not cap it (the only models that earn a layer entry).
func modelCap(limits caps.Limits, model string) *int {
	if v, ok := limits.Model[model]; ok {
		return &v
	}
	return nil
}
