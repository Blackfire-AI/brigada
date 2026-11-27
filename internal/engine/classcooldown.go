package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The class-scoped reliability cooldown (§8): repeated contract failures
// on one class — a review's stall, a missing or malformed verdict, an
// approve without substantive findings — cool the agent on that class
// alone, for a short window that doubles with each further consecutive
// failure. A valid completion on the class resets the streak, the window
// and the backoff step. The state is per agent and class (class_cooldown,
// migration 0082): the account-quota cooling_until on agents is global to
// the account and says nothing about a class, and the outcome record
// keeps only attempts and successes.
//
// The quarantine is an availability rule, not a cap: the placement
// pipeline reads the cooldown beside the agent's account cooling
// (qualifyingAgentRowsTx folds the later deadline into the candidate's
// cooling), so a quarantined agent never wins a lease — whatever its cost
// weight — yet stays in the unavailable lookup the cooling hold and the
// park reasons read, and a job whose agents are all quarantined waits for
// the earliest expiry like one whose agents are all cooling.
const (
	// classCooldownAfter is how many consecutive contract failures on
	// one class start the cooldown: the first is noise, the second a
	// pattern.
	classCooldownAfter = 2
	// classCooldownBase is the first cooldown window; each further
	// consecutive failure doubles it.
	classCooldownBase = 2 * time.Minute
	// classCooldownMax caps the doubled window: a reliability cooldown
	// is a short hold, never a broken state.
	classCooldownMax = 30 * time.Minute
)

// recordClassFailureTx advances the agent's consecutive contract-failure
// streak on the class and, from classCooldownAfter failures on, cools it
// on that class for the backoff window: the base doubled once per failure
// past the threshold, capped. The streak counts across jobs and projects
// — the class is the agent's, not one task's — and only a valid
// completion resets it (resetClassCooldownTx). An agent-less run has
// nobody to cool.
func recordClassFailureTx(tx *sql.Tx, agent int64, class string, now time.Time) error {
	if agent == 0 || class == "" {
		return nil
	}
	var streak int
	err := tx.QueryRow("SELECT streak FROM class_cooldown WHERE agent_id = ? AND class = ?", agent, class).Scan(&streak)
	if errors.Is(err, sql.ErrNoRows) {
		streak = 0
	} else if err != nil {
		return fmt.Errorf("read the %s failure streak of agent %d: %w", class, agent, err)
	}
	streak++
	until, step := "", 0
	if streak >= classCooldownAfter {
		step = streak - classCooldownAfter
		delay := classCooldownMax
		if step < 8 {
			if d := classCooldownBase << uint(step); d < classCooldownMax {
				delay = d
			}
		}
		until = fmtTime(now.Add(delay))
	}
	if _, err := tx.Exec(`INSERT INTO class_cooldown (agent_id, class, streak, cooling_until, step, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, class) DO UPDATE SET
			streak = excluded.streak, cooling_until = excluded.cooling_until, step = excluded.step, updated_at = excluded.updated_at`,
		agent, class, streak, until, step, fmtTime(now)); err != nil {
		return fmt.Errorf("record the %s contract failure of agent %d: %w", class, agent, err)
	}
	return nil
}

// resetClassCooldownTx clears the agent's contract-failure state on the
// class: a valid completion — a substantive review verdict — answers the
// contract, so the streak, the cooldown and the backoff step all start
// over. A failure on another class keeps its own state.
func resetClassCooldownTx(tx *sql.Tx, agent int64, class string) error {
	if agent == 0 || class == "" {
		return nil
	}
	if _, err := tx.Exec("DELETE FROM class_cooldown WHERE agent_id = ? AND class = ?", agent, class); err != nil {
		return fmt.Errorf("reset the %s cooldown of agent %d: %w", class, agent, err)
	}
	return nil
}
