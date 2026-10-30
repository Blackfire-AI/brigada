package engine

import (
	"database/sql"
	"errors"
	"fmt"
)

// The review placement's family steps (§7, review.family = prefer): a
// reviewer is picked in three ordered steps, the first with an available
// agent winning — an agent of a known different family than the author's,
// then another agent of the author's own family, then the author's own
// agent. Under review.family = require only the cross-family step exists:
// today's wall, where an agent of the author's family never qualifies.
const (
	familyStepCross  = 0 // an agent of a known different family
	familyStepSame   = 1 // another agent of the author's known family
	familyStepAuthor = 2 // the author's own agent
)

// The placement kinds a job_agent_picked event records for a review job
// (§7, issue #126): the family relationship of the reviewer the pick took.
// An unknown relationship — the author's family, or the reviewer's, cannot
// be told — is never labelled cross_family.
const (
	placementCrossFamily = "cross_family"
	placementSameFamily  = "same_family"
	placementSameAgent   = "same_agent"
	placementUnknown     = "unknown"
)

// reviewPlacement is one review job's family rule resolved (§7,
// review.family): whose work the review reads, the family witness the job
// carries, and whether another family is required or preferred.
type reviewPlacement struct {
	family  string // the author's family; "" = unknown
	author  int64  // the author's agent; 0 = none or agent-less
	require bool   // review.family = require
}

// stepOf places an agent in the family steps. ok is false when the
// placement never takes the agent: under require an agent of the author's
// family — or of an unknown family, when the author's is known — never
// qualifies, and the author itself never does; under prefer an unknown
// family never stands in as another family while the author's is known,
// and the author's own agent ranks last, taken only when nothing else can
// serve the review.
func (p reviewPlacement) stepOf(id int64, family string) (step int, ok bool) {
	if !p.require {
		switch {
		case p.family == "":
			// The author's family is unknown: any review-class agent
			// serves, the author's own agent last.
			if id == p.author {
				return familyStepAuthor, true
			}
			return familyStepCross, true
		case id == p.author:
			return familyStepAuthor, true
		case family == "":
			return familyStepCross, false
		case family == p.family:
			return familyStepSame, true
		default:
			return familyStepCross, true
		}
	}
	// require: the cross-family wall of §7 as it always was.
	if id == p.author {
		return familyStepCross, false
	}
	if p.family != "" && (family == "" || family == p.family) {
		return familyStepCross, false
	}
	return familyStepCross, true
}

// reviewPlacementTx resolves a review job's family placement (§7,
// review.family): whose work the review reads — the latest work or fix
// job's agent for a task review, the branch's last work job's for a
// standalone review, the project's foreman agent for a plan review —
// beside the family witness the job carries (ExcludeFamily, named at
// submit) and the setting's mode.
func reviewPlacementTx(tx *sql.Tx, cfg *Config, j *jobRow) (reviewPlacement, error) {
	p := reviewPlacement{family: j.ExcludeFamily, require: cfg.Review.Family == ReviewFamilyRequire}
	switch j.Kind {
	case KindReview:
		author, err := reviewAuthorTx(tx, j)
		if err != nil {
			return p, err
		}
		p.author = author
	case KindPlanReview:
		author, err := foremanAgentTx(tx, j.ProjectID)
		if err != nil {
			return p, err
		}
		p.author = author
	}
	return p, nil
}

// foremanAgentTx reads the project's foreman agent (§6.2): the one
// registered for the foreman harness and model the project chose. 0 when
// the project chose no whole pair or no runner advertises it. A plan
// review's "author" is this agent.
func foremanAgentTx(tx *sql.Tx, project int64) (int64, error) {
	var harness, model string
	if err := tx.QueryRow("SELECT foreman_harness, foreman_model FROM projects WHERE id = ?", project).
		Scan(&harness, &model); err != nil {
		return 0, fmt.Errorf("read the foreman of project %d: %w", project, err)
	}
	if harness == "" || model == "" {
		return 0, nil
	}
	var id int64
	err := tx.QueryRow("SELECT id FROM agents WHERE harness = ? AND model = ? ORDER BY id LIMIT 1", harness, model).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the foreman's agent of project %d: %w", project, err)
	}
	return id, nil
}

// reviewPlacementKind names the family relationship of a picked reviewer
// (§7, issue #126): cross_family, same_family, same_agent — or unknown,
// when the author's family, or the reviewer's, cannot be told. Computed at
// the pick alone, never earlier, so the event records what the placement
// really took.
func reviewPlacementKind(tx *sql.Tx, j *jobRow, reviewer int64, reviewerFamily string) (string, error) {
	var author int64
	var err error
	switch j.Kind {
	case KindReview:
		author, err = reviewAuthorTx(tx, j)
	case KindPlanReview:
		author, err = foremanAgentTx(tx, j.ProjectID)
	}
	if err != nil {
		return "", err
	}
	if author != 0 && reviewer == author {
		return placementSameAgent, nil
	}
	authorFamily := j.ExcludeFamily
	if author != 0 {
		// The live family of the author's agent, when it still has one,
		// beats the submit-time witness; a branch review whose family
		// came from a trailer keeps the witness, its only source.
		if live, err := agentFamilyTx(tx, author); err != nil {
			return "", err
		} else if live != "" {
			authorFamily = live
		}
	}
	switch {
	case authorFamily == "" || reviewerFamily == "":
		return placementUnknown, nil
	case reviewerFamily != authorFamily:
		return placementCrossFamily, nil
	default:
		return placementSameFamily, nil
	}
}
