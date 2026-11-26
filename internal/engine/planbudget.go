package engine

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The plan budget (§6.3): the cost rule that stands in place of the task
// count by default. A project sets approval_budget to a whole number of
// tokens, a dollar amount, or both joined with "+" — "2000000", "$5",
// "2000000+$5" — and a plan whose recorded spend passes it holds its new
// jobs: every task that would newly start parks at the plan gate, the
// amendment hold's park, with a needs-you line naming the rule and the
// spend, while work already running goes on.
//
// The spend is the usage the engine already records per job, summed over
// the plan's jobs: tokens are input plus output (input counts every
// input, cache reads included), dollars only what the job's harness
// reported. Unknown counters stay out of the sums, never zero: a harness
// without a price — a subscription login — counts tokens and no dollars,
// so a plan on one never trips a dollar budget.

// parseBudgetValue reads one approval_budget value: a whole number of
// tokens, a dollar amount, or both joined with "+", in either order. It
// returns the normalized spelling the settings row stores, the token
// count first: "2000000+$5".
func parseBudgetValue(key, value string) (string, int64, float64, error) {
	fail := func() (string, int64, float64, error) {
		return "", 0, 0, fmt.Errorf("%w: %s takes a whole number of tokens, a $ amount, or both joined with + (for example 2000000+$5), got %q",
			ErrInvalidSetting, key, value)
	}
	var tokens int64
	var usd float64
	haveTokens, haveUSD := false, false
	parts := strings.Split(value, "+")
	if len(parts) > 2 {
		return fail()
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "$") {
			if haveUSD {
				return fail()
			}
			v, err := strconv.ParseFloat(strings.TrimPrefix(part, "$"), 64)
			if err != nil || v <= 0 || v > 1e9 {
				return fail()
			}
			usd, haveUSD = v, true
			continue
		}
		if haveTokens {
			return fail()
		}
		v, err := strconv.ParseInt(part, 10, 64)
		if err != nil || v <= 0 {
			return fail()
		}
		tokens, haveTokens = v, true
	}
	switch {
	case haveTokens && haveUSD:
		return fmt.Sprintf("%d+$%s", tokens, budgetUSD(usd)), tokens, usd, nil
	case haveTokens:
		return strconv.FormatInt(tokens, 10), tokens, 0, nil
	case haveUSD:
		return "$" + budgetUSD(usd), 0, usd, nil
	}
	return fail()
}

// budgetUSD renders a dollar amount without trailing zeros.
func budgetUSD(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// planBudget is a parsed approval_budget.
type planBudget struct {
	tokens int64
	usd    float64
}

// projectBudget parses the stored approval_budget spelling; nil when
// none stands or a hand-edited row does not parse — the setting path
// validates, so only a hand edit lands there, and it counts as none.
func projectBudget(stored string) *planBudget {
	if stored == "" {
		return nil
	}
	_, tokens, usd, err := parseBudgetValue(SettingApprovalBudget, stored)
	if err != nil {
		return nil
	}
	return &planBudget{tokens: tokens, usd: usd}
}

// planSpendTx sums the usage recorded per job over one plan's jobs:
// input plus output tokens, and dollars. Unknown counters stay out of
// the sums, never zero.
func planSpendTx(tx *sql.Tx, plan int64) (tokens int64, usd float64, err error) {
	row := tx.QueryRow(`SELECT COALESCE(SUM(j.input_tokens), 0) + COALESCE(SUM(j.output_tokens), 0),
		COALESCE(SUM(j.cost_usd), 0)
		FROM jobs j JOIN tasks t ON t.id = j.task_id WHERE t.plan_id = ?`, plan)
	if err := row.Scan(&tokens, &usd); err != nil {
		return 0, 0, fmt.Errorf("sum the spend of plan %d: %w", plan, err)
	}
	return tokens, usd, nil
}

// planActuals is a plan's recorded usage for the done report (§6.2):
// what the jobs' harnesses reported, and the rounds the engine records
// itself. The harness-reported counters are nil when no job reported
// them — unknown, never zero — while restarts and rounds are always
// known.
type planActuals struct {
	tokens   *int64
	usd      *float64
	wall     *float64
	restarts int64
	rounds   int64
}

// planActualsTx reads a plan's actuals from its jobs' records (§6.2):
// input plus output tokens, dollars, and wall time — each nil when no
// job's harness reported the counter, the budget rule's convention
// (planSpendTx sums only what the harness reported) — and the restarts
// and fix/review rounds its jobs spent, which the engine records itself
// and so always knows.
func planActualsTx(tx *sql.Tx, plan int64) (planActuals, error) {
	var a planActuals
	var tokens sql.NullInt64
	var usd, wall sql.NullFloat64
	err := tx.QueryRow(`SELECT
		SUM(CASE WHEN j.input_tokens IS NOT NULL OR j.output_tokens IS NOT NULL
			THEN COALESCE(j.input_tokens, 0) + COALESCE(j.output_tokens, 0) END),
		SUM(j.cost_usd),
		SUM(j.wall_seconds),
		COALESCE(SUM(CASE WHEN j.restart = 1 AND j.retry = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN j.kind = 'fix' AND j.round IN (?, ?) THEN 1 ELSE 0 END), 0)
		FROM jobs j JOIN tasks t ON t.id = j.task_id WHERE t.plan_id = ?`,
		roundFix, roundReview, plan).Scan(&tokens, &usd, &wall, &a.restarts, &a.rounds)
	if err != nil {
		return a, fmt.Errorf("sum the actuals of plan %d: %w", plan, err)
	}
	if tokens.Valid {
		a.tokens = &tokens.Int64
	}
	if usd.Valid {
		a.usd = &usd.Float64
	}
	if wall.Valid {
		a.wall = &wall.Float64
	}
	return a, nil
}

// planActualText renders the done report's actuals (§6.2): the plan's
// spend and rounds, the harness-reported counters shown only when some
// job reported them — unknown stays out, never zero — and the restarts
// and rounds, which the engine records itself, always shown.
func planActualText(a planActuals) string {
	var parts []string
	switch {
	case a.tokens != nil && a.usd != nil:
		parts = append(parts, fmt.Sprintf("%d tokens and $%s", *a.tokens, budgetUSD(*a.usd)))
	case a.tokens != nil:
		parts = append(parts, fmt.Sprintf("%d tokens", *a.tokens))
	case a.usd != nil:
		parts = append(parts, "$"+budgetUSD(*a.usd))
	}
	if a.wall != nil {
		parts = append(parts, wallText(*a.wall)+" wall")
	}
	parts = append(parts, countNoun(a.restarts, "restart"))
	parts = append(parts, countNoun(a.rounds, "round"))
	return strings.Join(parts, ", ")
}

// countNoun renders a count with the singular or plural of its noun:
// "1 restart", "2 restarts".
func countNoun(n int64, singular string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %ss", n, singular)
}

// wallText renders the done report's wall time (§6.2): a whole-second
// span as "1m30s", "45m" or "30s", never "45m0s".
func wallText(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second)).Round(time.Second)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case h > 0 && m > 0 && s > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0 && s > 0:
		return fmt.Sprintf("%dh%ds", h, s)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	case m > 0 && s > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// budgetGate is one plan's budget decision, read once for a wave of
// tasks about to start. A nil gate is no budget: nothing parks.
type budgetGate struct {
	b          *planBudget
	tokens     int64
	usd        float64
	heldReason string
}

// planBudgetGateTx reads the project's budget and the plan's spend
// against it. A nil gate without error is no budget standing.
func planBudgetGateTx(tx *sql.Tx, project, plan int64) (*budgetGate, error) {
	settings, err := projectApprovalTx(tx, project)
	if err != nil {
		return nil, err
	}
	b := projectBudget(settings.Budget)
	if b == nil {
		return nil, nil
	}
	tokens, usd, err := planSpendTx(tx, plan)
	if err != nil {
		return nil, err
	}
	return &budgetGate{b: b, tokens: tokens, usd: usd, heldReason: budgetHoldReason(b, tokens, usd)}, nil
}

// over reports whether the spend passed the budget.
func (g *budgetGate) over() bool { return g != nil && g.heldReason != "" }

// parkTx holds one task the spend passed the budget for: it parks at the
// plan gate with a needs-you line naming the rule and the spend, and no
// job is queued. It reports whether it parked.
func (g *budgetGate) parkTx(tx *sql.Tx, project, task int64, now time.Time) (bool, error) {
	if !g.over() {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE tasks SET state = ?, updated_at = ? WHERE id = ?`,
		taskNeedsForeman, fmtTime(now), task); err != nil {
		return false, fmt.Errorf("hold task %d over the plan budget: %w", task, err)
	}
	if _, err := appendEventTx(tx, evTaskPlanHeld, project, task, 0,
		eventPayload{Status: taskNeedsForeman, Reason: g.heldReason}, now); err != nil {
		return false, err
	}
	return true, nil
}

// budgetHoldReason names the approval_budget rule the plan's spend
// passed, "" when it passed none.
func budgetHoldReason(b *planBudget, tokens int64, usd float64) string {
	overTokens := b.tokens > 0 && tokens > b.tokens
	overUSD := b.usd > 0 && usd > b.usd
	if !overTokens && !overUSD {
		return ""
	}
	var spend, limit string
	switch {
	case overTokens && overUSD:
		spend = fmt.Sprintf("%d tokens and $%s", tokens, budgetUSD(usd))
		limit = fmt.Sprintf("%d tokens and $%s", b.tokens, budgetUSD(b.usd))
	case overTokens:
		spend = fmt.Sprintf("%d tokens", tokens)
		limit = fmt.Sprintf("%d tokens", b.tokens)
	default:
		spend = "$" + budgetUSD(usd)
		limit = "$" + budgetUSD(b.usd)
	}
	return fmt.Sprintf("plan spend %s, over the approval_budget of %s", spend, limit)
}
