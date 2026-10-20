package engine

import (
	"database/sql"
	"fmt"
	"slices"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// The review.tier settings (§7, §8, decided 2026-10-20): a review asks for
// the tier below its task's effective tier, or for the same tier.
const (
	ReviewTierBelow = "below"
	ReviewTierSame  = "same"
)

// defaultReviewEffort is the effort a review runs at when neither the task
// nor the config names one.
const defaultReviewEffort = "medium"

// ReviewConfig is the engine-wide default for reviews (§7, §8): the config
// file's review.tier and review.effort. The zero value is the default,
// one tier below the task's at medium effort.
type ReviewConfig struct {
	Tier   string // ReviewTierBelow or ReviewTierSame; '' = below
	Effort string // low, medium, high or max; '' = medium
}

// ValidReviewTier reports whether word is a review.tier setting.
func ValidReviewTier(word string) bool {
	return word == ReviewTierBelow || word == ReviewTierSame
}

// reviewTierFor is the tier a review of a task asks for (§8): the task's
// own review tier when it has one, else the effective tier of the task
// stepped down by the default. Light stays light.
func (c ReviewConfig) reviewTierFor(effective, own string) string {
	if own != "" {
		return own
	}
	if c.Tier == ReviewTierSame {
		return effective
	}
	if at := slices.Index(adapter.Tiers, effective); at > 0 {
		return adapter.Tiers[at-1]
	}
	return effective
}

// reviewEffortFor is the effort a review runs at: the task's own review
// effort, else the default. The task's work effort, escalated or not, never
// counts.
func (c ReviewConfig) reviewEffortFor(own string) string {
	switch {
	case own != "":
		return own
	case c.Effort != "":
		return c.Effort
	}
	return defaultReviewEffort
}

// jobTierTx is the tier a task job asks for (§8): the task's effective tier
// for a work or fix job, the review tier for a review.
func jobTierTx(tx *sql.Tx, rv ReviewConfig, j *jobRow) (string, error) {
	var effective, own string
	if err := tx.QueryRow("SELECT effective_tier, review_tier FROM tasks WHERE id = ?", j.TaskID).Scan(&effective, &own); err != nil {
		return "", fmt.Errorf("read the tier of task %d: %w", j.TaskID, err)
	}
	if j.Kind != KindReview {
		return effective, nil
	}
	return rv.reviewTierFor(effective, own), nil
}

// jobEffortTx is the brigada effort level a task job runs at, before the
// harness maps it: the task's effective effort, else its own, for a work or
// fix job — possibly empty, the agent's own level then —, and the review
// effort for a review.
func jobEffortTx(tx *sql.Tx, rv ReviewConfig, j *jobRow) (string, error) {
	if j.Kind == KindReview {
		var own string
		if err := tx.QueryRow("SELECT review_effort FROM tasks WHERE id = ?", j.TaskID).Scan(&own); err != nil {
			return "", fmt.Errorf("read the review effort of task %d: %w", j.TaskID, err)
		}
		return rv.reviewEffortFor(own), nil
	}
	var effort string
	if err := tx.QueryRow("SELECT COALESCE(NULLIF(effective_effort, ''), effort) FROM tasks WHERE id = ?", j.TaskID).Scan(&effort); err != nil {
		return "", fmt.Errorf("read the effort of task %d: %w", j.TaskID, err)
	}
	return effort, nil
}
