package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
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
	// ForemanEffort is the project's chosen foreman effort level, set on a
	// foreman turn's grant only; '' = none chosen, the runner agent's own
	// effort stands.
	ForemanEffort string

	Harness string // the job's harness constraint; '' = any (§8)
	Agent   string // the harness of the agent the lease picked (§8); '' = none
	// AgentAdapter and AgentModel name the picked agent exactly, as a
	// foreman turn's grant carries them: a registration is identified by
	// its runner, adapter and model, and the harness alone can name two of
	// them. Empty on every other kind of grant, whose runner resolves the
	// agent by its harness.
	AgentAdapter string
	AgentModel   string
	RepoURL      string // the task's own repo, else the project's (§6.1)
	BaseRef      string // the ref the worktree starts from; for a review, the diff's base end (§7)
	CheckCmd     string // the check the runner runs after a done (§5.5); '' = none — always for a review
	// Setup is the environment step the runner runs before the worker and
	// again before the check (§5.7); nil = the project had none.
	Setup *SetupSpec
	// ExtraRepos are the read-only repositories the runner checks out
	// beside the worktree (§5.8): the task's recorded list, the project's
	// default for a task-less review.
	ExtraRepos []ExtraRepo
	// ReviewBranch is the task's result branch a review job reviews
	// (§7): the runner's worktree sits at it, fetched from the remote,
	// and it is all the review reads — never pushed, never re-made.
	// Empty for every kind but review: a work job's worktree makes its
	// own branch. BaseRef and ReviewBranch together name the diff the
	// review prompt tells the reviewer to run.
	ReviewBranch string
	// Branch is the branch the engine names a work or fix job to work
	// on (§5.1): a work job makes it from the base ref, a fix round
	// continues the task's current branch, and the runner builds and
	// pushes exactly it — never a name of its own choosing. Named once,
	// at the job's first lease, and kept by every later lease of the
	// same job. Empty for a review, which reads ReviewBranch and moves
	// nothing, and for a foreman turn, which belongs to no task.
	Branch  string
	Attempt int64 // leases taken on the job, this one included
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
		// A poll shows the runner is here, before the sweep can call it
		// stale (§8 "Health").
		if err := e.runnerSeenTx(tx, owner, now); err != nil {
			return err
		}
		// What it offers is what the sweep's busy-runner rule reads to
		// tell whether it could take a labelled job.
		offered := labels
		if offered == nil {
			offered = []string{}
		}
		labelsJSON, err := json.Marshal(offered)
		if err != nil {
			return fmt.Errorf("encode labels: %w", err)
		}
		offeredHarnesses := harnesses
		if offeredHarnesses == nil {
			offeredHarnesses = []string{}
		}
		harnessesJSON, err := json.Marshal(offeredHarnesses)
		if err != nil {
			return fmt.Errorf("encode harnesses: %w", err)
		}
		if _, err := tx.Exec("UPDATE runners SET labels = ?, harnesses = ? WHERE id = ?",
			string(labelsJSON), string(harnessesJSON), owner); err != nil {
			return fmt.Errorf("record the offer of runner %d: %w", owner, err)
		}
		if _, err := e.sweepPolledTx(tx, now, owner); err != nil {
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
	// After a recover nothing is leased until one lease-expiry window has
	// passed (§6.4); the jobs wait queued, the sweep above has already
	// requeued the leases the lost store held.
	if held, err := e.dispatchHeldTx(tx, now); err != nil {
		return nil, err
	} else if held {
		return nil, nil
	}
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
			if err := e.foremanFinishedTx(tx, j, statusFailed, now); err != nil {
				return nil, err
			}
			if j.TaskID != 0 {
				if err := e.taskAfterCompletionTx(tx, j, Completion{Error: reason, ErrorKind: "runner_lost"}, now); err != nil {
					return nil, err
				}
			}
			continue
		}
		// The harness the job is placed on: a foreman turn's is resolved
		// from its pair (a turn submitted without a harness runs on the
		// project's), and a turn with no whole pair is held — no poll takes
		// it, and none reserves capacity for it.
		jobHarness := j.Harness
		if j.Kind == KindForeman {
			pair, err := foremanPairTx(tx, j)
			if err != nil {
				return nil, err
			}
			if !pair.ok() {
				continue
			}
			jobHarness = pair.harness
		}
		if jobHarness != "" && !slices.Contains(harnesses, jobHarness) {
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

		cands, err := leaseAgentsTx(tx, owner, j, nowStr)
		if err != nil {
			return nil, err
		}
		// The agent rules (§3, §7, §8): a job that excludes a family or
		// an agent needs one of this runner's agents that passes the
		// exclusions, review-capable; a job that requires an agent — a
		// fix round on the author's session — needs that one. A runner
		// with none — no agents at all, only the author's family, only
		// the excluded reviewer, not the author's agent, no agent that
		// takes the job's class, or none ready and out of cooling — does
		// not get the job. That is an unmet placement, not a cap hold:
		// nothing is stamped, and the queue TTL measures the job's wait
		// like any other's. A runner with no agents at all is the
		// exception, and only for a job that carries no placement of its
		// own: it leases with nothing recorded, as before — a class
		// filter over an empty advertisement would leave such a runner
		// nothing ever.
		if len(cands) == 0 {
			// A foreman turn is placed on its pair: a runner with no agent
			// for it, agentless included, does not get it.
			becauseplaced := j.Kind == KindForeman || j.ExcludeFamily != "" || j.ExcludeAgent != 0 || j.RequireAgent != 0
			if !becauseplaced {
				var has bool
				if has, err = runnerHasAgentsTx(tx, owner); err != nil {
					return nil, err
				}
				becauseplaced = has
			}
			if becauseplaced {
				continue
			}
			cands = []agentCand{{}} // no agent recorded
		}
		// The rank choice (§8): the best-scored agent, or — by the
		// exploration share, when a next-best exists — the next-best, so a
		// weaker agent keeps being measured.
		walk, pick := cands, pickBest
		if len(cands) > 1 && e.explore() {
			walk = append([]agentCand{cands[1], cands[0]}, cands[2:]...)
			pick = pickExploration
		}
		// The cap gate (§8.1): the job starts only if every layer on its
		// path has room. The walk takes the first agent in pick order
		// whose path has room, so a full account does not hold the job
		// while a lower-ranked agent has room. A full layer on every
		// agent holds the job in place — it stays queued and the loop
		// moves on, so a later job on a layer with room can still be
		// leased — and the hold, stamped on the job from the first agent
		// walked, is what keeps the queue TTL from failing it (§8.1).
		// What a held turn reserved, a task job may not take in the same
		// call (§8.1): it waits like any held job.
		var chosen *agentCand
		var cjob, holdJob caps.Job
		var holdLayer string
		var holdByRoom bool
		for i := range walk {
			c := &walk[i]
			cj, err := capsJobTx(tx, owner, c.id, advertised, j, e.cfg.Caps)
			if err != nil {
				return nil, err
			}
			layer, byRoom := "", false
			if room, l := caps.Room(e.cfg.Caps, usage, cj); !room {
				layer, byRoom = l, true
			} else if j.Kind != KindForeman {
				layer, _ = reserved.layer(cj)
			}
			if layer == "" {
				chosen, cjob = c, cj
				break
			}
			if i == 0 {
				holdJob, holdLayer, holdByRoom = cj, layer, byRoom
			}
		}
		if chosen == nil {
			if err := holdAtCapTx(tx, j, holdJob, holdLayer, now); err != nil {
				return nil, err
			}
			// A held turn reserves the layer it waits on for the rest
			// of this call: the next slot that frees there goes to it,
			// not to a task job leasing behind it in the same poll.
			if holdByRoom && j.Kind == KindForeman {
				reserved.reserve(holdLayer, holdJob.Account)
			}
			continue
		}
		agent, agentHarness := chosen.id, chosen.harness
		leaseID, err := e.cfg.NewLeaseID(owner)
		if err != nil {
			return nil, err
		}
		// The branch a job works on is the engine's to name (§5.1), and
		// it is named once: the first lease computes it and the column
		// keeps it, so a re-lease after a lost lease works on the same
		// branch as the lease before it.
		jobBranch := j.Branch
		if jobBranch == "" {
			jobBranch, err = jobBranchTx(tx, j)
			if err != nil {
				return nil, err
			}
		}
		expires := now.Add(e.cfg.LeaseTTL)
		_, err = tx.Exec(`UPDATE jobs SET status = 'leased', lease_id = ?, lease_owner = ?, lease_expires_at = ?,
			lease_epoch = ?, fence_token = attempts + 1, attempts = attempts + 1, agent_id = ?,
			branch = COALESCE(branch, NULLIF(?, '')),
			first_leased = COALESCE(first_leased, ?), last_leased = ?, queued_at = NULL, last_event = NULL,
			cap_held_at = NULL, cap_held_layer = NULL, cap_held_key = NULL, slot_hold = 0,
			released_by = NULL, released_until = NULL, released_lease = NULL, updated_at = ?
			WHERE id = ?`,
			leaseID, owner, fmtTime(expires), epoch, nullIfZero(agent), jobBranch, nowStr, nowStr, nowStr, j.ID)
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
			// The session generation this lease runs under: the completion
			// writes its session id back only while it is still current.
			if _, err := tx.Exec("UPDATE jobs SET session_gen = COALESCE((SELECT foreman_session_gen FROM projects WHERE id = ?), 0) WHERE id = ?",
				j.ProjectID, j.ID); err != nil {
				return nil, fmt.Errorf("stamp the session generation of job %d: %w", j.ID, err)
			}
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
		// Why this agent (§8): plain log, no inbox row and no wake. The
		// agents ranked above the pick that were passed over for cap room
		// are named; an exploration pick passed over the best on purpose,
		// which is not a cap matter, and a walk that fell back from the
		// explored agent to the best is a best pick.
		if agent != 0 && j.Kind != KindForeman {
			var over []int64
			if pick == pickExploration && chosen.rank != 1 {
				pick = pickBest
			}
			if pick == pickBest {
				for _, c := range cands {
					if c.rank < chosen.rank {
						over = append(over, c.id)
					}
				}
			}
			if _, err := appendEventTx(tx, evJobAgentPicked, j.ProjectID, j.TaskID, j.ID,
				eventPayload{Agent: agent, Score: chosen.score, Pick: pick, PassedOver: over}, now); err != nil {
				return nil, err
			}
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
			if j.TaskID == 0 {
				// A standalone review has no task to hold any of it: the
				// project gives the repo and the diff's base (its default
				// branch), the job itself the branch it reads.
				repo, baseRef, err = standaloneReviewInfoTx(tx, j)
				if err != nil {
					return nil, err
				}
				reviewBranch = j.Branch
			}
			// A review names no branch of its own: it reads one.
			jobBranch = ""
		}
		// The environment step (§5.7): the task's recorded setup, for a
		// work, fix or review job alike; a task-less review has no
		// record, so the project's setting stands. A foreman turn has
		// none.
		var setup *SetupSpec
		var extraRepos []ExtraRepo
		switch {
		case j.TaskID != 0:
			if setup, err = taskSetupTx(tx, j.TaskID); err != nil {
				return nil, err
			}
			if extraRepos, err = taskExtraReposTx(tx, j.TaskID); err != nil {
				return nil, err
			}
		case j.Kind == KindReview:
			if setup, err = projectSetupTx(tx, j.ProjectID); err != nil {
				return nil, err
			}
			def, err := projectExtraReposTx(tx, j.ProjectID)
			if err != nil {
				return nil, err
			}
			extraRepos = def.repos
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
			// A session id belongs to one harness's CLI: a foreman turn
			// that runs on another harness than the project's current one
			// (a turn queued before a switch) starts fresh, never resuming
			// the session of the harness the project now runs.
			if resume != "" && j.Kind == KindForeman {
				var projectHarness string
				if err := tx.QueryRow("SELECT foreman_harness FROM projects WHERE id = ?", j.ProjectID).Scan(&projectHarness); err != nil && !errors.Is(err, sql.ErrNoRows) {
					return nil, fmt.Errorf("read the foreman of project %d: %w", j.ProjectID, err)
				}
				if projectHarness != agentHarness {
					resume = ""
				}
			}
		}
		foremanEffort := ""
		if j.Kind == KindForeman {
			// The project's chosen foreman effort rides a foreman turn's
			// grant, and overrides the runner agent's own for the turn. The
			// level was validated for the project's harness, so it rides only
			// when that is the harness the turn runs on: a turn queued before
			// a switch to another harness keeps its agent's own effort.
			var projectHarness, projectEffort string
			if err := tx.QueryRow("SELECT foreman_harness, foreman_effort FROM projects WHERE id = ?", j.ProjectID).Scan(&projectHarness, &projectEffort); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("read the foreman effort of project %d: %w", j.ProjectID, err)
			}
			runsOn := agentHarness
			if runsOn == "" {
				runsOn = j.Harness // no agent recorded: the job's own constraint
			}
			if projectHarness == runsOn {
				foremanEffort = projectEffort
			}
		}
		agentAdapter, agentModel := "", ""
		if j.Kind == KindForeman {
			agentAdapter, agentModel = chosen.adapter, chosen.model
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

			ForemanEffort:   foremanEffort,
			Harness:         j.Harness,
			Agent:           agentHarness,
			AgentAdapter:    agentAdapter,
			AgentModel:      agentModel,
			RepoURL:         repo,
			BaseRef:         baseRef,
			CheckCmd:        checkCmd,
			Setup:           setup,
			ExtraRepos:      extraRepos,
			ReviewBranch:    reviewBranch,
			Branch:          jobBranch,
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

// The rank choices a lease records (§8).
const (
	pickBest        = "best"
	pickExploration = "exploration"
)

// agentCand is one eligible agent of the lease's ranking (§8): its
// harness rides beside the id so the grant can name it, its score is the
// outcome record's for the job's (repo, class), and rank is its place in
// the ranking, 0 the best.
type agentCand struct {
	id      int64
	adapter string // the adapter the agent was registered under: with the model, its identity on its runner
	harness string
	score   float64
	rank    int

	// Filled for every lookup, but read only for the agents that cannot
	// take the job yet (qualifyingAgentsTx): model, health, cooling_until.
	model   string
	health  string
	cooling string
}

// explore rolls the exploration share (§8): true when this pick should
// take the next-best agent. A share of 0 never rolls.
func (e *Engine) explore() bool {
	share := *e.cfg.ExploreShare
	return share > 0 && e.cfg.Rand() < share
}

// leaseAgentsTx ranks the agents the lease may pick from (§8, §8.1): the
// runner's agents matching the job's harness constraint, ready and out
// of cooling at now, offering the job kind's class, and passing the
// family rule's exclusions when it carries them (§7, §8) — best score
// first for the job's (repo, class), ties in id order, so a runner with
// no record at all ranks by id. A job that requires an agent (§3) — a
// fix round resuming the author's session — has its list named
// outright: that agent or none, never ranked against another. Empty
// when the runner advertises no such agent — the caller then refuses
// the lease, unless the runner advertises nothing at all.
func leaseAgentsTx(tx *sql.Tx, runner int64, j *jobRow, now string) ([]agentCand, error) {
	return eligibleAgentsTx(tx, runner, j, kindClass(j.Kind), now)
}

// eligibleAgentsTx shares eligibility and ranking with automatic retries,
// whose candidate lookup uses the task's class rather than the job kind's.
// Runner 0 ranks all registered agents for an automatic restart's pin.
func eligibleAgentsTx(tx *sql.Tx, runner int64, j *jobRow, class, now string) ([]agentCand, error) {
	return qualifyingAgentsTx(tx, runner, j, class, now, false)
}

// qualifyingAgentsTx is eligibleAgentsTx's lookup. With unavailable
// true it also returns the agents that are broken or cooling at now —
// those that qualify for the job by pin, harness, class and family rule
// but cannot take it yet — with their model, health and cooling
// deadline filled in: what the cooling hold (§8.1) and the queue-TTL
// park reason read.
func qualifyingAgentsTx(tx *sql.Tx, runner int64, j *jobRow, class, now string, unavailable bool) ([]agentCand, error) {
	// A foreman turn runs on the harness AND model its project chose: only
	// an agent offering exactly that pair qualifies, and a turn whose pair
	// cannot be resolved has no agent at all.
	harness, model := j.Harness, ""
	if j.Kind == KindForeman {
		pair, err := foremanPairTx(tx, j)
		if err != nil {
			return nil, err
		}
		if !pair.ok() {
			return nil, nil
		}
		harness, model = pair.harness, pair.model
	}
	query := `SELECT a.id, a.family, a.harness, a.classes, a.model, a.health, COALESCE(a.cooling_until, ''), COALESCE(o.attempts, 0), COALESCE(o.successes, 0), a.adapter
		FROM agents a LEFT JOIN outcomes o ON o.agent_id = a.id AND o.project_id = ? AND o.class = ?
		WHERE 1 = 1`
	args := []any{j.ProjectID, class}
	if !unavailable {
		query += " AND a.health = 'ready' AND (a.cooling_until IS NULL OR a.cooling_until <= ?)"
		args = append(args, now)
	}
	if runner != 0 {
		query += " AND a.runner_id = ?"
		args = append(args, runner)
	}
	if harness != "" {
		query += " AND a.harness = ?"
		args = append(args, harness)
	}
	if model != "" {
		query += " AND a.model = ?"
		args = append(args, model)
	}
	if j.RequireAgent != 0 {
		query += " AND a.id = ?"
		args = append(args, j.RequireAgent)
	}
	rows, err := tx.Query(query+" ORDER BY a.id", args...)
	if err != nil {
		return nil, fmt.Errorf("pick the agent of runner %d: %w", runner, err)
	}
	defer rows.Close()
	var cands []agentCand
	for rows.Next() {
		var id int64
		var attempts, successes int
		var family, harness, classes, model, health, cooling, adapterName string
		if err := rows.Scan(&id, &family, &harness, &classes, &model, &health, &cooling, &attempts, &successes, &adapterName); err != nil {
			return nil, fmt.Errorf("pick the agent of runner %d: %w", runner, err)
		}
		// Ordinary work and fix leases use the implement class. A pinned
		// work restart already qualified against the task's class when
		// the retry was queued; do not reject that pin for lacking implement.
		pinnedRetry := j.Kind == KindWork && j.Restart && j.RequireAgent != 0
		if class != "" && !pinnedRetry && !agentOffersClass(classes, class) {
			continue
		}
		if id == j.ExcludeAgent {
			continue
		}
		if j.ExcludeFamily != "" || (j.Kind == KindReview && j.ExcludeAgent != 0) {
			if !reviewAgentQualifies(id, family, classes, j.ExcludeFamily, j.ExcludeAgent) {
				continue
			}
		}
		cands = append(cands, agentCand{id: id, adapter: adapterName, harness: harness, model: model, health: health, cooling: cooling, score: Score(attempts, successes)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pick the agent of runner %d: %w", runner, err)
	}
	sort.SliceStable(cands, func(x, y int) bool { return cands[x].score > cands[y].score })
	for i := range cands {
		cands[i].rank = i
	}
	return cands, nil
}

// kindClass is the dispatch class for ordinary leases (§8): work and fix
// use implement regardless of the task's class; review and foreman jobs
// use review and answer respectively.
func kindClass(k Kind) string {
	switch k {
	case KindWork, KindFix:
		return "implement"
	case KindReview:
		return string(KindReview)
	case KindForeman:
		return "answer"
	}
	return ""
}

// runnerHasAgentsTx reports whether the runner advertised any agents at
// all. The lease's refusal rule turns on it: a runner with agents that
// offers none for a job does not take it, while a runner with no
// advertisement leases with nothing recorded, as it always has.
func runnerHasAgentsTx(tx *sql.Tx, runner int64) (bool, error) {
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM agents WHERE runner_id = ?", runner).Scan(&n); err != nil {
		return false, fmt.Errorf("count the agents of runner %d: %w", runner, err)
	}
	return n > 0, nil
}

// reviewAgentQualifies is the review placement's agent test (§7, §8):
// another family than the excluded one, not the excluded agent, and
// offering the review class. The excluded family and agent are the
// author's and — on a retry — the first reviewer's; either left empty
// (an agent-less author or reviewer) excludes nothing.
func reviewAgentQualifies(id int64, family, classes, excludeFamily string, excludeAgent int64) bool {
	switch {
	case id == excludeAgent:
		return false
	case excludeFamily != "" && family == excludeFamily:
		return false
	}
	return agentOffersClass(classes, string(KindReview))
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
	if _, err := tx.Exec(`UPDATE jobs SET cap_held_at = ?, cap_held_layer = ?, cap_held_key = ?, slot_hold = 0, updated_at = ? WHERE id = ?`,
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
// no task; its reading checkout uses the project repo and default branch.
func taskInfoTx(tx *sql.Tx, j *jobRow) (repoURL, baseRef, checkCmd, branch string, err error) {
	if j.TaskID == 0 {
		if j.Kind == KindForeman {
			repoURL, baseRef, err = standaloneReviewInfoTx(tx, j)
			return repoURL, baseRef, "", "", err
		}
		return "", "", "", "", nil
	}
	var override string
	err = tx.QueryRow(`SELECT COALESCE(t.repo_url, ''), COALESCE(NULLIF(p.transport_url, ''), p.repo_url), t.base_ref, COALESCE(t.check_cmd, ''), COALESCE(t.branch, '')
		FROM tasks t JOIN plans pl ON pl.id = t.plan_id JOIN projects p ON p.id = pl.project_id
		WHERE t.id = ?`, j.TaskID).
		Scan(&override, &repoURL, &baseRef, &checkCmd, &branch)
	if err != nil {
		return "", "", "", "", fmt.Errorf("read task %d: %w", j.TaskID, err)
	}
	if override != "" {
		// The task's own URL is the transport the runner fetches with
		// (§5.1), as given. A legacy override the canonical rules refuse
		// (an ambiguous or credential-looking form) is never handed on:
		// the grant carries no repo, so the runner fails the job with
		// "no repo URL" instead of a git error quoting the string into a
		// failure result and its event.
		if _, perr := Canonicalize(override); perr != nil {
			return "", baseRef, checkCmd, branch, nil
		}
		repoURL = override
	}
	return repoURL, baseRef, checkCmd, branch, nil
}

// standaloneReviewInfoTx reads what a task-less review's grant needs
// from the project (§7): its repo URL, and its default branch as the
// base end of the diff. Foreman turns use the same project metadata
// for their reading checkout.
func standaloneReviewInfoTx(tx *sql.Tx, j *jobRow) (repoURL, baseRef string, err error) {
	if err := tx.QueryRow("SELECT COALESCE(NULLIF(transport_url, ''), repo_url), default_branch FROM projects WHERE id = ?", j.ProjectID).
		Scan(&repoURL, &baseRef); err != nil {
		return "", "", fmt.Errorf("read project %d: %w", j.ProjectID, err)
	}
	return repoURL, baseRef, nil
}

// jobBranchTx names the branch a work or fix job works on (§5.1, §5.6):
// the task's current branch for a fix round — the round continues the
// work the task's completion recorded — and for a work job a name of
// the task's own: brigada/task-<id> for its first attempt, and
// brigada/task-<id>-r<n> after the task's n-th restart, counted over
// the task's restart jobs, this one included, so each restart works on
// a branch no earlier attempt pushed to and its push from a fresh base
// is a fast-forward of its own branch. A fix round on a task that has
// no branch yet — nothing of its work completed to record one — takes
// the work rule's name: the branch its work starts on. A review reads
// the task's branch without moving it (§7) and a foreman turn belongs
// to no task; both name nothing.
func jobBranchTx(tx *sql.Tx, j *jobRow) (string, error) {
	if j.Kind != KindWork && j.Kind != KindFix {
		return "", nil
	}
	if j.Kind == KindFix {
		var branch string
		if err := tx.QueryRow("SELECT COALESCE(branch, '') FROM tasks WHERE id = ?", j.TaskID).
			Scan(&branch); err != nil {
			return "", fmt.Errorf("read the branch of task %d: %w", j.TaskID, err)
		}
		if branch != "" {
			return branch, nil
		}
	}
	var restarts int64
	if err := tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", j.TaskID).
		Scan(&restarts); err != nil {
		return "", fmt.Errorf("count the restarts of task %d: %w", j.TaskID, err)
	}
	if restarts == 0 {
		return fmt.Sprintf("brigada/task-%d", j.TaskID), nil
	}
	return fmt.Sprintf("brigada/task-%d-r%d", j.TaskID, restarts), nil
}
