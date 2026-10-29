package engine

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// The placement kinds a review job's pick records (issue #126): how the
// chosen reviewer relates to the review's author. An unknown
// relationship — the author's family could not be told — is never
// labelled cross_family.
const (
	placeCrossFamily = "cross_family" // a known family different from the author's known one
	placeSameFamily  = "same_family"  // the author's known family, another agent
	placeSameAgent   = "same_agent"   // the author's own agent
	placeUnknown     = "unknown"      // the author's family is unknown
)

// reviewAuthorFamilyTx reads whose work a review judges and that agent's
// family: the latest work or fix job's agent for a task review, the
// branch's last work job's agent for a standalone one, and the project's
// foreman agent for a plan review, whose author the foreman is (§6.2,
// §7). Zero with an empty family when no agent is known; a known agent
// may still carry no family.
func reviewAuthorFamilyTx(tx *sql.Tx, j *jobRow) (int64, string, error) {
	if j.Kind == KindPlanReview {
		var harness, model string
		if err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", j.ProjectID).
			Scan(&harness, &model); err != nil {
			return 0, "", fmt.Errorf("read the foreman of project %d: %w", j.ProjectID, err)
		}
		if harness == "" || model == "" {
			return 0, "", nil
		}
		var id int64
		var family string
		err := tx.QueryRow("SELECT id, family FROM agents WHERE harness = ? AND model = ? ORDER BY id LIMIT 1", harness, model).
			Scan(&id, &family)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", nil
		}
		if err != nil {
			return 0, "", fmt.Errorf("read the foreman's agent of project %d: %w", j.ProjectID, err)
		}
		return id, family, nil
	}
	author, err := reviewAuthorTx(tx, j)
	if err != nil || author == 0 {
		return author, "", err
	}
	family, err := agentFamilyTx(tx, author)
	if err != nil {
		return 0, "", err
	}
	return author, family, nil
}

// reviewAuthorKnownFamilyTx is reviewAuthorFamilyTx's family with the
// job's own exclusion as the witness of last resort: a family learned
// from a Co-Authored-By trailer (§7) has no agent behind it, and the
// job's exclude_family column is its only record.
func reviewAuthorKnownFamilyTx(tx *sql.Tx, j *jobRow) (int64, string, error) {
	author, family, err := reviewAuthorFamilyTx(tx, j)
	if err != nil {
		return 0, "", err
	}
	if family == "" {
		family = j.ExcludeFamily
	}
	return author, family, nil
}

// preferReviewAgentsTx places a review under review.family=prefer
// (issue #126): three ordered family steps — an agent of a known
// different family than the author's first, then an agent of the
// author's own family other than the author's, then the author's own
// agent — and the first step with an available reviewer wins, judged
// over every runner's agents together so a cross-family reviewer on any
// runner beats a same-family one on the polling runner. The tier
// fallback (§8) runs within the winning step alone: a cross-family
// reviewer on any tier beats a same-family one on the asked tier, and
// with no step available the job waits on the preferred step's soonest
// reset. An author whose family is unknown keeps today's rule instead:
// any review-class agent but the author's own. A pinned job (a
// re-review on its reviewer's session) has its agent for the winning
// step whatever its family. runner 0 ranks over every registered agent.
func preferReviewAgentsTx(tx *sql.Tx, cfg *Config, rnd func() float64, runner int64, j *jobRow, class, now string, unavailable bool) ([]agentCand, error) {
	author, authorFamily, err := reviewAuthorKnownFamilyTx(tx, j)
	if err != nil {
		return nil, err
	}
	rows, err := qualifyingAgentRowsTx(tx, cfg.Review.Family, 0, j, class, now)
	if err != nil {
		return nil, err
	}
	steps := reviewFamilySteps(rows, author, authorFamily)
	winning, first := -1, -1
	for i, step := range steps {
		if len(step) == 0 {
			continue
		}
		if first < 0 {
			first = i
		}
		if stepAvailable(cfg.Caps, step, now) {
			winning = i
			break
		}
	}
	if winning < 0 {
		winning = first // no step available: wait on the preferred one
	}
	if winning < 0 {
		return nil, nil
	}
	step := steps[winning]
	asked := ""
	if j.TaskID != 0 && j.Kind != KindForeman {
		if asked, err = jobTierTx(tx, cfg.Review, j); err != nil {
			return nil, err
		}
	} else if j.Kind == KindPlanReview {
		// A plan review (§6.2) has no task to name a tier: the
		// project's plan_review.tier asks, as a task review's own ask.
		if asked, err = planReviewTierTx(tx, cfg, j); err != nil {
			return nil, err
		}
	}
	served := ""
	if j.RequireAgent == 0 && asked != "" {
		served = tierServedRows(cfg.Caps, step, asked, now)
	}
	var cands []agentCand
	for _, c := range step {
		if runner != 0 && c.runner != runner {
			continue
		}
		tier := cfg.Caps.TierOf(c.harness, c.model)
		if served != "" && tier != served {
			continue
		}
		if !unavailable && (c.health != "ready" || c.cooling > now) {
			continue
		}
		if asked != "" {
			c.tierAsked, c.tierServed = asked, tier
		}
		c.weight = cfg.Caps.CostOf(c.harness, c.model)
		cands = append(cands, c)
	}
	if j.RequireAgent != 0 {
		rnd = nil // a pinned job names its agent: nothing to rank or draw
	}
	rankAgents(cands, rnd)
	return cands, nil
}

// reviewFamilySteps splits a review's qualifying agents into the ordered
// prefer steps (issue #126): a known different family first, then the
// author's family on another agent, then the author's own agent. An
// author of unknown family keeps one step alone — every qualifying
// agent, which today's rule already keeps the author out of.
func reviewFamilySteps(rows []agentCand, author int64, authorFamily string) [][]agentCand {
	if authorFamily == "" {
		return [][]agentCand{rows}
	}
	steps := make([][]agentCand, 3)
	for _, c := range rows {
		switch {
		case c.id == author:
			steps[2] = append(steps[2], c)
		case c.family == authorFamily:
			steps[1] = append(steps[1], c)
		default:
			steps[0] = append(steps[0], c)
		}
	}
	return steps
}

// stepAvailable reports whether any agent of one family step is available
// now — ready, not cooling and not capped at 0, the meaning the tier
// fallback's ready set already has (§8).
func stepAvailable(limits caps.Limits, step []agentCand, now string) bool {
	for _, a := range step {
		if a.health == "ready" && a.cooling <= now && !cappedAtZero(limits, a.account, a.model) {
			return true
		}
	}
	return false
}

// reviewPlacementKindTx names how a picked reviewer relates to the
// review's author (issue #126): cross_family, same_family, same_agent —
// or unknown when the author's family is not known. Computed at the
// pick, never earlier: the registrations may have changed while the job
// queued. agent 0 (no agent recorded) names nothing.
func reviewPlacementKindTx(tx *sql.Tx, j *jobRow, agent int64) (string, error) {
	if agent == 0 {
		return "", nil
	}
	author, authorFamily, err := reviewAuthorKnownFamilyTx(tx, j)
	if err != nil {
		return "", err
	}
	if authorFamily == "" {
		return placeUnknown, nil
	}
	if agent == author {
		return placeSameAgent, nil
	}
	family, err := agentFamilyTx(tx, agent)
	if err != nil {
		return "", err
	}
	switch {
	case family == "":
		return placeUnknown, nil
	case family == authorFamily:
		return placeSameFamily, nil
	default:
		return placeCrossFamily, nil
	}
}

// placementSQL reads, for the job the SQL expression jobExpr names, the
// placement its latest job_agent_picked event recorded (issue #126):
// the kind of a review that was actually leased. A review still queued
// or waiting has no such event and so no placement.
func placementSQL(jobExpr string) string {
	return `COALESCE((SELECT json_extract(pe.payload, '$.placement') FROM events pe
		WHERE pe.kind = 'job_agent_picked' AND pe.job_id = ` + jobExpr + ` ORDER BY pe.id DESC LIMIT 1), '')`
}

// shownPlacement is the placement the status and the words that reach
// the foreman or the user name: empty for cross_family (the engine
// default, left unsaid) and for no placement at all.
func shownPlacement(kind string) string {
	if kind == placeCrossFamily {
		return ""
	}
	return kind
}

// placementNote names a review's placement once, in the words the
// needs-you reasons and the foreman's wake lines append; empty when
// there is nothing to say.
func placementNote(kind string) string {
	if kind = shownPlacement(kind); kind == "" {
		return ""
	}
	return " (review placement: " + kind + ")"
}
