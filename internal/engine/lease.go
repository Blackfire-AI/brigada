package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// LeaseGrant is a job plus the lease that took it: what a runner needs
// to run the job and to speak about it afterwards (§12.3). The task
// fields — RepoURL, BaseRef, CheckCmd — are what the runner's workspace
// and check are made of (§5.1, §5.5); Attempt is the lease count after
// this one, so a re-dispatched attempt can take a fresh branch name
// (§6.4).
type LeaseGrant struct {
	JobID      int64
	LeaseOwner int64 // the runner the lease belongs to
	ProjectID  int64
	TaskID     int64 // 0 for a foreman turn
	Kind       Kind
	Mode       Mode
	Key        string
	Prompt     string
	LeaseID    string
	// LeaseExpiresAt is when the lease lapses without a heartbeat.
	LeaseExpiresAt time.Time
	// ForemanToken is the lease-scoped foreman token (§12.1, §12.2),
	// minted with the lease of a foreman turn and empty for every task
	// job. It is the secret's only appearance: it reaches the runner in
	// the grant and lives in the runner's memory alone — never a log,
	// an event or an error message, and the store keeps only its hash.
	ForemanToken string

	Harness  string // the job's harness constraint; '' = any (§8)
	RepoURL  string // the task's own repo, else the project's (§6.1)
	BaseRef  string // the ref the worktree starts from; for a review, the diff's base end (§7)
	CheckCmd string // the check the runner runs after a done (§5.5); '' = none — always for a review
	// ReviewBranch is the task's result branch a review job reviews
	// (§7): the runner's worktree sits at it, fetched from the remote,
	// and it is all the review reads — never pushed, never re-made.
	// Empty for every kind but review: a work job's worktree makes its
	// own branch. BaseRef and ReviewBranch together name the diff the
	// review prompt tells the reviewer to run.
	ReviewBranch string
	Attempt      int64 // leases taken on the job, this one included
	// ResumeSessionID is the harness session the key's last run ended
	// on (§4.2), for a session-mode job to resume; empty when the key
	// never ran or the job is fresh.
	ResumeSessionID string
}

// Lease hands the caller the oldest queued job it may run, or nil when
// there is none (§12.3, §8). A job is offered to owner only if the
// owner offers its harness and all its labels, is not the runner that
// just released it (until the exclusion lapses), and — for a
// session-mode job — is not a different runner than the one the key's
// live session sits on. A job whose path is full at a concurrency cap
// is skipped and stays queued, waiting for a slot (§8.1). A pending
// foreman turn outranks every queued task job (§8.1): turns come
// first, oldest first among themselves, and the layers a held turn
// waits on are closed to task jobs for the rest of the call.
func (e *Engine) Lease(ctx context.Context, owner int64, harnesses, labels []string) (*LeaseGrant, error) {
	var grant *LeaseGrant
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := sweepTx(tx, e.cfg, now); err != nil {
			return err
		}
		g, err := e.leaseTx(tx, owner, harnesses, labels, now)
		if err != nil {
			return fmt.Errorf("lease for runner %d: %w", owner, err)
		}
		grant = g
		return nil
	})
	return grant, err
}

func (e *Engine) leaseTx(tx *sql.Tx, owner int64, harnesses, labels []string, now time.Time) (*LeaseGrant, error) {
	epoch, err := epochTx(tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT ` + jobColumns + ` FROM jobs WHERE status = 'queued' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list queued jobs: %w", err)
	}
	var queued []*jobRow
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("list queued jobs: %w", err)
		}
		queued = append(queued, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list queued jobs: %w", err)
	}
	rows.Close()

	// The gate's two fixed inputs (§8.1): the live jobs counted per
	// layer, built once — a lease below adds to it, so a later decision
	// in this same call sees the slot it took — and the cap this runner
	// advertised, folded with the config's by caps.RunnerCap.
	usage, err := capsUsageTx(tx)
	if err != nil {
		return nil, err
	}
	var advertised int
	if err := tx.QueryRow("SELECT COALESCE((SELECT max_parallel FROM runners WHERE id = ?), 0)", owner).Scan(&advertised); err != nil {
		return nil, fmt.Errorf("read runner %d: %w", owner, err)
	}

	// The foreman is never starved (§8.1): a pending foreman turn
	// outranks every queued task job, so the poll walks the queue with
	// turns first — oldest first within each group, the order the query
	// already read. Task jobs among themselves keep their oldest-first
	// order, and a queue with no turn in it is walked exactly as
	// before.
	ordered := make([]*jobRow, 0, len(queued))
	for _, j := range queued {
		if j.Kind == KindForeman {
			ordered = append(ordered, j)
		}
	}
	for _, j := range queued {
		if j.Kind != KindForeman {
			ordered = append(ordered, j)
		}
	}

	// reserved keeps, for this call alone, the layer instances a
	// foreman turn this runner could take is held at.
	var reserved capReservation
	nowStr := fmtTime(now)
	for _, j := range ordered {
		// A job that lost its runner MaxAttempts times fails here rather
		// than being offered again — at lease time, not in the sweep, so
		// a heartbeat can still revive a healthy third lease (§3). This
		// only fires when some runner asks for work; until then the job
		// simply sits queued, and the queue TTL — not this rule — is
		// what frees its key.
		if e.cfg.MaxAttempts > 0 && j.Attempts >= int64(e.cfg.MaxAttempts) {
			reason := fmt.Sprintf("runner lost %d times", e.cfg.MaxAttempts)
			if err := failJobTx(tx, j.ID, reason, 0, now); err != nil {
				return nil, err
			}
			if _, err := appendEventTx(tx, evJobAttemptsExceeded, j.ProjectID, j.TaskID, j.ID,
				eventPayload{Status: statusFailed, Reason: reason}, now); err != nil {
				return nil, err
			}
			continue
		}
		if j.Harness != "" && !slices.Contains(harnesses, j.Harness) {
			continue
		}
		if !labelsCover(labels, jobLabels(j)) {
			continue
		}
		// A job this owner released goes to other runners first; the
		// exclusion lapses so a lone runner can retry (§12.3).
		if j.ReleasedBy == owner && j.ReleasedUntil != "" && j.ReleasedUntil > nowStr {
			continue
		}
		if j.Mode == ModeSession {
			free, err := affinityFreeTx(tx, j.Key, owner)
			if err != nil {
				return nil, err
			}
			if !free {
				continue
			}
		}

		agent, err := leaseAgentTx(tx, owner, j)
		if err != nil {
			return nil, err
		}
		// The family rule (§7, §8): a job that excludes a family needs
		// one of this runner's agents of another family, review-capable.
		// A runner with none — no agents at all, or only the author's
		// family — does not get the job. That is an unmet placement, not
		// a cap hold: nothing is stamped, and the queue TTL measures the
		// job's wait like any other's.
		if j.ExcludeFamily != "" && agent == 0 {
			continue
		}
		// The cap gate (§8.1): the job starts only if every layer on its
		// path has room. A full layer holds the job in place — it stays
		// queued and the loop moves on, so a later job on a layer with
		// room can still be leased — and the hold is stamped on the job,
		// which is what keeps the queue TTL from failing it (§8.1).
		cjob, err := capsJobTx(tx, owner, agent, advertised, j)
		if err != nil {
			return nil, err
		}
		if room, layer := caps.Room(e.cfg.Caps, usage, cjob); !room {
			if err := holdAtCapTx(tx, j, cjob, layer, now); err != nil {
				return nil, err
			}
			// A held turn reserves the layer it waits on for the rest
			// of this call: the next slot that frees there goes to it,
			// not to a task job leasing behind it in the same poll.
			if j.Kind == KindForeman {
				reserved.reserve(layer, cjob.Account)
			}
			continue
		}
		// What a held turn reserved, a task job may not take in the
		// same call (§8.1): it waits like any held job, its queue TTL
		// protected by the hold the reservation stamps.
		if j.Kind != KindForeman {
			if layer, held := reserved.layer(cjob); held {
				if err := holdAtCapTx(tx, j, cjob, layer, now); err != nil {
					return nil, err
				}
				continue
			}
		}
		leaseID, err := e.cfg.NewLeaseID(owner)
		if err != nil {
			return nil, err
		}
		expires := now.Add(e.cfg.LeaseTTL)
		_, err = tx.Exec(`UPDATE jobs SET status = 'leased', lease_id = ?, lease_owner = ?, lease_expires_at = ?,
			lease_epoch = ?, fence_token = attempts + 1, attempts = attempts + 1, agent_id = ?,
			first_leased = COALESCE(first_leased, ?), last_leased = ?, queued_at = NULL,
			cap_held_at = NULL, cap_held_layer = NULL, cap_held_key = NULL,
			released_by = NULL, released_until = NULL, released_lease = NULL, updated_at = ?
			WHERE id = ?`,
			leaseID, owner, fmtTime(expires), epoch, nullIfZero(agent), nowStr, nowStr, nowStr, j.ID)
		if err != nil {
			return nil, fmt.Errorf("lease job %d: %w", j.ID, err)
		}
		usage.Add(cjob.Runner, cjob.Account, cjob.Model, cjob.Plan)
		if j.Mode == ModeSession {
			// The session now sits on this runner: later session jobs on
			// the key are invisible to the others (§8).
			if err := upsertSessionTx(tx, j.Key, owner, "", now); err != nil {
				return nil, err
			}
		}
		// A foreman turn's lease mints its scoped token (§12.1): the
		// secret rides the grant, and only its hash the store.
		foremanToken := ""
		if j.Kind == KindForeman {
			secret, err := e.mintLeaseTokenTx(tx, j, leaseID, expires, now)
			if err != nil {
				return nil, err
			}
			foremanToken = secret
		}
		if _, err := appendEventTx(tx, evJobLeased, j.ProjectID, j.TaskID, j.ID,
			eventPayload{Lease: leasePrefix(leaseID), Owner: owner, Attempt: j.Attempts + 1}, now); err != nil {
			return nil, err
		}
		// The task leaves ready while one of its jobs is leased (§6.1);
		// a task already past ready keeps the state it reached.
		if j.TaskID != 0 {
			if err := taskRunningTx(tx, j, now); err != nil {
				return nil, err
			}
		}
		repo, baseRef, checkCmd, branch, err := taskInfoTx(tx, j)
		if err != nil {
			return nil, err
		}
		reviewBranch := ""
		if j.Kind == KindReview {
			// The review reads the task's result branch (§7) and runs no
			// check: the task's check already spoke, at the work round
			// this review judges, so the grant carries the branch alone.
			reviewBranch = branch
			checkCmd = ""
		}
		resume := ""
		if j.Mode == ModeSession {
			// What the key's last run ended on, if it left a session id
			// behind (§4.2); empty for the key's first session job.
			var sid sql.NullString
			if err := tx.QueryRow("SELECT harness_session_id FROM sessions WHERE key = ?", j.Key).Scan(&sid); err == nil && sid.Valid {
				resume = sid.String
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("read the session of %s: %w", j.Key, err)
			}
		}
		return &LeaseGrant{
			JobID:          j.ID,
			LeaseOwner:     owner,
			ProjectID:      j.ProjectID,
			TaskID:         j.TaskID,
			Kind:           j.Kind,
			Mode:           j.Mode,
			Key:            j.Key,
			Prompt:         j.Prompt,
			LeaseID:        leaseID,
			LeaseExpiresAt: expires,
			ForemanToken:   foremanToken,

			Harness:         j.Harness,
			RepoURL:         repo,
			BaseRef:         baseRef,
			CheckCmd:        checkCmd,
			ReviewBranch:    reviewBranch,
			Attempt:         j.Attempts + 1,
			ResumeSessionID: resume,
		}, nil
	}
	return nil, nil // nothing this runner may take
}

// capReservation is what one lease poll holds back for waiting foreman
// turns (§8.1): the layer instances at which a queued foreman job this
// runner could take is held. It lives in memory for the current
// leaseTx call alone — the next poll recomputes it from the store —
// and while a layer instance stays reserved, no task job in the same
// call may lease through it, or it would fill the slot the turn is
// waiting for.
type capReservation struct {
	global  bool            // a turn is held at the global cap
	runner  bool            // a turn is held at this runner's own cap
	account map[string]bool // by account label: a turn held at that account's cap
}

// reserve adds the layer a foreman turn was held at. Room holds a turn
// only at the layers it counts toward (§8.1) — runner, account and
// global — so any other layer reserves nothing.
func (r *capReservation) reserve(layer, account string) {
	switch layer {
	case caps.LayerGlobal:
		r.global = true
	case caps.LayerRunner:
		r.runner = true
	case caps.LayerAccount:
		if r.account == nil {
			r.account = map[string]bool{}
		}
		r.account[account] = true
	}
}

// layer reports the first reserved layer instance the dispatch would
// run through, in the order Room names layers full (§8.1), and whether
// there is one. A reserved runner layer is always the polling runner's
// own: a lease in this call would run on no other.
func (r capReservation) layer(job caps.Job) (string, bool) {
	switch {
	case r.runner:
		return caps.LayerRunner, true
	case job.Account != "" && r.account[job.Account]:
		return caps.LayerAccount, true
	case r.global:
		return caps.LayerGlobal, true
	}
	return "", false
}

// leaseAgentTx picks the agent a lease records on the job (§8.1): the
// runner's first agent — by id — matching the job's harness constraint,
// or the runner's first agent when the job leaves the harness open.
// Under the family rule (§7, §8) — a job that excludes a family, the
// review job of an implement task — only the runner's agents of another
// family that offer the review class qualify, and the first of those by
// id is the pick. 0 when the runner advertises no such agent: the job
// then runs with nothing to count by at the account and model layers,
// except that the family rule's caller refuses the lease outright.
func leaseAgentTx(tx *sql.Tx, runner int64, j *jobRow) (int64, error) {
	query := "SELECT id, family, classes FROM agents WHERE runner_id = ?"
	args := []any{runner}
	if j.Harness != "" {
		query += " AND harness = ?"
		args = append(args, j.Harness)
	}
	rows, err := tx.Query(query+" ORDER BY id", args...)
	if err != nil {
		return 0, fmt.Errorf("pick the agent of runner %d: %w", runner, err)
	}
	defer rows.Close()
	var pick int64
	for rows.Next() {
		var id int64
		var family, classes string
		if err := rows.Scan(&id, &family, &classes); err != nil {
			return 0, fmt.Errorf("pick the agent of runner %d: %w", runner, err)
		}
		if j.ExcludeFamily != "" &&
			(family == j.ExcludeFamily || !agentOffersClass(classes, string(KindReview))) {
			continue
		}
		pick = id
		break
	}
	return pick, rows.Err()
}

// agentOffersClass reports whether the agent's advertised classes (§5.1,
// a JSON array) include class. A classes column that will not parse
// offers nothing.
func agentOffersClass(classes, class string) bool {
	var offered []string
	if json.Unmarshal([]byte(classes), &offered) != nil {
		return false
	}
	return slices.Contains(offered, class)
}

// holdAtCapTx records that a full cap layer holds a queued job (§8.1).
// cap_held_at is stamped on every hold — the queue TTL counts from the
// later of queued_at and it, so a job runners keep finding cap-blocked
// waits as long as it takes — while the job_cap_held event lands only
// when the hold starts or moves to another layer, never per poll.
// cap_held_layer names the layer kind and cap_held_key the held instance
// — the polling runner's id, the account label, the model name or the
// plan id, empty for a global hold — so the capacity snapshot attributes
// the queued job to exactly the instance that had no room, never to a
// guess from the job's harness (§12.2).
func holdAtCapTx(tx *sql.Tx, j *jobRow, job caps.Job, layer string, now time.Time) error {
	nowStr := fmtTime(now)
	if _, err := tx.Exec(`UPDATE jobs SET cap_held_at = ?, cap_held_layer = ?, cap_held_key = ?, updated_at = ? WHERE id = ?`,
		nowStr, layer, holdKey(layer, job), nowStr, j.ID); err != nil {
		return fmt.Errorf("hold job %d at the %s cap: %w", j.ID, layer, err)
	}
	if j.CapHeldLayer == layer {
		return nil
	}
	if _, err := appendEventTx(tx, evJobCapHeld, j.ProjectID, j.TaskID, j.ID,
		eventPayload{Layer: layer}, now); err != nil {
		return err
	}
	return nil
}

// holdKey is the cap_held_key stamp for one hold (§12.2): the instance
// within the layer that had no room — the polling runner's id, the
// account label, the model name or the plan id. A global hold names no
// instance and stamps an empty key.
func holdKey(layer string, job caps.Job) string {
	switch layer {
	case caps.LayerRunner:
		return strconv.FormatInt(job.Runner, 10)
	case caps.LayerAccount:
		return job.Account
	case caps.LayerModel:
		return job.Model
	case caps.LayerPlan:
		return strconv.FormatInt(job.Plan, 10)
	default:
		return ""
	}
}

// taskInfoTx reads the workspace fields of a worker job's task: the
// repo URL (the task's own, else the project's, §6.1), the base ref,
// the check command the runner runs after a done (§5.5), and the
// result branch a review job reviews (§7). A foreman turn belongs to
// no task and gets empty fields.
func taskInfoTx(tx *sql.Tx, j *jobRow) (repoURL, baseRef, checkCmd, branch string, err error) {
	if j.TaskID == 0 {
		return "", "", "", "", nil
	}
	err = tx.QueryRow(`SELECT COALESCE(NULLIF(t.repo_url, ''), p.repo_url), t.base_ref, COALESCE(t.check_cmd, ''), COALESCE(t.branch, '')
		FROM tasks t JOIN plans pl ON pl.id = t.plan_id JOIN projects p ON p.id = pl.project_id
		WHERE t.id = ?`, j.TaskID).
		Scan(&repoURL, &baseRef, &checkCmd, &branch)
	if err != nil {
		return "", "", "", "", fmt.Errorf("read task %d: %w", j.TaskID, err)
	}
	return repoURL, baseRef, checkCmd, branch, nil
}
