package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/Blackfire-AI/brigada/internal/adapter"
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

// preferServedTier picks the tier a review under review.family=prefer is
// served at (§7, §8, issues #126, #263): the capability floor bounds the
// family order. The asked tier is a minimum, never stepped down. Within
// it a cross-family reviewer beats a same-family one; only when the asked
// tier has no available reviewer does a higher tier come into play, its
// cross-family reviewers before its same-family ones, and the author's own
// agent last of all, at an eligible tier in a fresh session. The first
// group with an available reviewer (ready, not cooling, not capped at 0)
// wins, judged over every runner's agents together so a cross-family
// reviewer on any runner beats a same-family one on the polling runner.
// With no group available the job waits on the earliest reset the floor
// allows (tierServedRows). An author whose family is unknown keeps one
// group — any review-class agent but the author's own — and the plain
// floor chooses its tier. A reviewer capped at exactly 0 is already left
// out by the caller (dropZeroCapped, issue #195).
func preferServedTier(limits caps.Limits, foreman map[string]bool, rows []agentCand, author int64, authorFamily, asked, now string) string {
	floor := slices.Index(adapter.Tiers, asked)
	if authorFamily == "" || floor < 0 {
		return tierServedRows(limits, foreman, rows, asked, now)
	}
	askedReady := false
	higherCross := map[string]bool{}
	higherSame := map[string]bool{}
	authorTiers := map[string]bool{}
	for _, a := range rows {
		if a.health != "ready" || a.cooling > now {
			continue
		}
		tier := limits.TierOf(a.harness, a.model)
		at := slices.Index(adapter.Tiers, tier)
		if at < floor {
			continue // below the floor: never a placement
		}
		switch {
		case a.id == author:
			authorTiers[tier] = true
		case at == floor:
			askedReady = true // a cross- or same-family reviewer at the asked tier
		case a.family != authorFamily:
			higherCross[tier] = true
		default:
			higherSame[tier] = true
		}
	}
	if askedReady {
		return asked
	}
	for _, tier := range adapter.Tiers[floor+1:] {
		if higherCross[tier] {
			return tier
		}
	}
	for _, tier := range adapter.Tiers[floor+1:] {
		if higherSame[tier] {
			return tier
		}
	}
	for _, tier := range adapter.Tiers[floor:] {
		if authorTiers[tier] {
			return tier
		}
	}
	return tierServedRows(limits, foreman, rows, asked, now)
}

// orderPreferCandidates orders a prefer review's candidates by the family
// preference within the served tier (§7, issue #263): a cross-family
// reviewer first, then a same-family one, then the author's own agent,
// each group ranked by the cost-weighted sampling (rankAgents). The cap
// gate walks this order, so a cap-full preferred family falls through to
// the next family group at the same tier under prefer, never to another
// tier. An author of unknown family keeps one group, ranked alone.
func orderPreferCandidates(cands []agentCand, rnd func() float64, author int64, authorFamily string) []agentCand {
	if authorFamily == "" {
		rankAgents(cands, rnd)
		return cands
	}
	var groups [3][]agentCand
	for _, c := range cands {
		g := preferFamilyClass(c.id, c.family, author, authorFamily)
		groups[g] = append(groups[g], c)
	}
	var out []agentCand
	for _, g := range groups {
		rankAgents(g, rnd)
		out = append(out, g...)
	}
	for i := range out {
		out[i].rank = i
	}
	return out
}

// preferFamilyClass names a reviewer's family group under prefer: 0 a
// known family other than the author's, 1 the author's family on another
// agent, 2 the author's own agent.
func preferFamilyClass(id int64, family string, author int64, authorFamily string) int {
	switch {
	case id == author:
		return 2
	case family == authorFamily:
		return 1
	default:
		return 0
	}
}

// preferWinningClass names the first family group with an available reviewer
// at the served tier (every runner's agents together): the group the lease
// places, and the one a polling runner must offer to take the job at all.
// An empty served tier reads every tier, as a standalone review's does.
func preferWinningClass(limits caps.Limits, rows []agentCand, author int64, authorFamily, served, now string) int {
	best := -1
	for _, a := range rows {
		if a.health != "ready" || a.cooling > now {
			continue
		}
		if served != "" && limits.TierOf(a.harness, a.model) != served {
			continue
		}
		if c := preferFamilyClass(a.id, a.family, author, authorFamily); best < 0 || c < best {
			best = c
		}
	}
	return best
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
