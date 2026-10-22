package engine

import (
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// An answer to a hold can change the rule (§6.3): the user's yes on a
// held plan or landing may also stop the rule asking again, through the
// same code path as `brigada project set`, recorded in the history. Every
// hold reason maps to exactly one setting, defined here in one place.

// The hold reasons the gates give (§6.3).
const (
	holdGatePlan = "gate_plan" // gate_plan is ask: every plan waits
	holdGateLand = "gate_land" // gate_land is ask: every landing waits
	holdMaxTasks = "max_tasks" // the plan's task count is over approval_max_tasks
	holdBudget   = "budget"    // the plan's spend passed approval_budget (planbudget.go)
	holdClass    = "class"     // a class outside approval_classes
	holdPath     = "path"      // a protected path of approval_protected_paths
	holdTier     = "tier"      // a tier of approval_tiers
	holdRestart  = "restart"   // a spent restart under approval_restarted
	holdExtras   = "extras"    // an extra repository outside the project default
)

// holdSettingOf maps every hold reason to the one setting whose change
// stops it asking again (§6.3).
var holdSettingOf = map[string]string{
	holdGatePlan: SettingGatePlan,
	holdGateLand: SettingGateLand,
	holdMaxTasks: SettingApprovalMaxTasks,
	holdBudget:   SettingApprovalBudget,
	holdClass:    SettingApprovalClasses,
	holdPath:     SettingApprovalProtectedPaths,
	holdTier:     SettingApprovalTiers,
	holdRestart:  SettingApprovalRestarted,
	holdExtras:   SettingExtraRepos,
}

// holdAsk is one hold reason in the structured form the setting change
// reads: the kind, the item it names (a class, a path pattern, a tier,
// an extra repository), the task count a max_tasks hold frees, and the
// spend a budget hold frees.
type holdAsk struct {
	kind  string
	item  string
	extra *ExtraRepo
	count int
	toks  int64
	usd   float64
}

// word is the hold's phrase, as the needs-you line and the hold's stored
// reason say it.
func (a holdAsk) word() string {
	switch a.kind {
	case holdGatePlan:
		return "gate_plan is " + GateAsk
	case holdGateLand:
		return "gate_land is " + GateAsk
	case holdRestart:
		return "restarted"
	case holdTier:
		return a.item
	case holdPath:
		return "touches " + a.item
	default:
		return a.item
	}
}

// stopAskingValues computes the key=value pairs that stop the given hold
// reasons from asking again: each rule moves just far enough that the
// hold it answer no longer trips. The values take the spelling
// `brigada project set` takes.
func stopAskingValues(s approvalSettings, asks []holdAsk) map[string]string {
	out := map[string]string{}
	if len(asks) == 0 {
		return out
	}
	classes := append([]string{}, s.Classes...)
	paths := append([]string{}, s.ProtectedPaths...)
	tiers := append([]string{}, s.ApprovalTiers...)
	extras := append([]ExtraRepo{}, s.ExtraRepos...)
	limit := *s.MaxTasks
	count, budget := false, false
	var toks int64
	var usd float64
	drop := func(list []string, item string) []string {
		for i, x := range list {
			if x == item {
				return append(list[:i], list[i+1:]...)
			}
		}
		return list
	}
	for _, a := range asks {
		switch a.kind {
		case holdGatePlan:
			out[SettingGatePlan] = GateAuto
		case holdGateLand:
			out[SettingGateLand] = GateAuto
		case holdRestart:
			out[SettingApprovalRestarted] = "false"
		case holdClass:
			if !slices.Contains(classes, a.item) {
				classes = append(classes, a.item)
			}
		case holdPath:
			paths = drop(paths, a.item)
		case holdTier:
			tiers = drop(tiers, a.item)
		case holdExtras:
			if a.extra != nil && !extraRepoListed(extras, a.extra.Repo) {
				extras = append(extras, *a.extra)
			}
		case holdMaxTasks:
			count = true
			if a.count > limit {
				limit = a.count
			}
		case holdBudget:
			budget = true
			if a.toks > toks {
				toks = a.toks
			}
			if a.usd > usd {
				usd = a.usd
			}
		}
	}
	join := func(list []string) string { return strings.Join(list, settingListSep) }
	if !slices.Equal(classes, s.Classes) {
		out[SettingApprovalClasses] = join(classes)
	}
	if !slices.Equal(paths, s.ProtectedPaths) {
		if len(paths) == 0 {
			out[SettingApprovalProtectedPaths] = "" // the default: none
		} else {
			out[SettingApprovalProtectedPaths] = join(paths)
		}
	}
	if !slices.Equal(tiers, s.ApprovalTiers) {
		if len(tiers) == 0 {
			out[SettingApprovalTiers] = "" // the default: none
		} else {
			out[SettingApprovalTiers] = join(tiers)
		}
	}
	if len(extras) != len(s.ExtraRepos) {
		items := make([]string, len(extras))
		for i, x := range extras {
			items[i] = extraRepoText(x)
		}
		out[SettingExtraRepos] = join(items)
	}
	if count {
		out[SettingApprovalMaxTasks] = strconv.Itoa(limit)
	}
	if budget {
		v := ""
		switch {
		case toks > 0 && usd > 0:
			v = fmt.Sprintf("%d+$%s", toks, budgetUSD(usd))
		case toks > 0:
			v = strconv.FormatInt(toks, 10)
		case usd > 0:
			v = "$" + budgetUSD(usd)
		}
		if v != "" {
			out[SettingApprovalBudget] = v
		}
	}
	return out
}

func extraRepoListed(list []ExtraRepo, repo string) bool {
	for _, x := range list {
		if x.Repo == repo {
			return true
		}
	}
	return false
}

// settingPairs renders applied setting changes as sorted key=value
// strings, the shape the decisions report and the history records.
func settingPairs(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// stopAskingTx applies the setting changes the hold reasons' answer
// carries (§6.3) through the same path as `brigada project set`,
// recorded in the history. It returns the rendered key=value pairs.
func (e *Engine) stopAskingTx(tx *sql.Tx, project int64, s approvalSettings, asks []holdAsk, now time.Time) ([]string, error) {
	values := stopAskingValues(s, asks)
	if len(values) == 0 {
		return nil, nil
	}
	if err := e.applySettingsTx(tx, project, values, now); err != nil {
		return nil, err
	}
	return settingPairs(values), nil
}

// planHoldAsksTx names the rules that hold an awaiting plan (§6.3), in
// the structured form the setting change reads: the same evaluation the
// plan gate ran, on the plan's tasks as they stand. The land rules —
// approval_tiers, approval_restarted — never hold a plan, and the plan
// budget never holds an awaiting one (it has queued nothing, so it has
// spent nothing; the budget parks tasks of a running plan instead,
// planbudget.go), so no ask for them can reach this path: landApprove's
// own asks (landHoldAsksTx) carry the tier, the restart and the paths a
// landing trips.
func planHoldAsksTx(tx *sql.Tx, project, plan int64) (approvalSettings, []holdAsk, error) {
	s, err := projectApprovalTx(tx, project)
	if err != nil {
		return approvalSettings{}, nil, err
	}
	rows, err := tx.Query("SELECT spec, COALESCE(check_cmd, ''), COALESCE(class, ''), COALESCE(extra_repos, '') FROM tasks WHERE plan_id = ?", plan)
	if err != nil {
		return approvalSettings{}, nil, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
	}
	defer rows.Close()
	var tasks []PlanTask
	for rows.Next() {
		var spec, check, class, extra string
		if err := rows.Scan(&spec, &check, &class, &extra); err != nil {
			return approvalSettings{}, nil, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
		}
		t := PlanTask{Spec: spec, Check: check, Class: class}
		if spec2 := parseExtraReposRecord(extra); spec2 != nil {
			t.ExtraRepos = spec2.Repos
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return approvalSettings{}, nil, fmt.Errorf("read the tasks of plan %d: %w", plan, err)
	}
	v := s.evaluate(tasks)
	var asks []holdAsk
	if v.Gate {
		asks = append(asks, holdAsk{kind: holdGatePlan})
	}
	if v.Limit > 0 && v.Tasks > v.Limit {
		asks = append(asks, holdAsk{kind: holdMaxTasks, count: v.Tasks})
	}
	for _, c := range v.Classes {
		asks = append(asks, holdAsk{kind: holdClass, item: c})
	}
	for _, p := range v.Paths {
		asks = append(asks, holdAsk{kind: holdPath, item: p})
	}
	// The extra repositories, as entries the setting takes: the same
	// comparison the evaluation makes, kept by repository.
	inDefault := map[string]bool{}
	for _, x := range s.ExtraRepos {
		inDefault[x.Repo] = true
	}
	seen := map[string]bool{}
	for _, t := range tasks {
		for _, x := range t.ExtraRepos {
			if inDefault[x.Repo] || seen[x.Repo] {
				continue
			}
			seen[x.Repo] = true
			y := x
			asks = append(asks, holdAsk{kind: holdExtras, extra: &y})
		}
	}
	return s, asks, nil
}
