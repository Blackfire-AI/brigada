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

	"github.com/Blackfire-AI/brigada/internal/adapter"
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
	// Effort is the task job's mapped effort, with the agent default applied.
	Effort string
	// ForemanEffort is the project's chosen foreman effort level, set on a
	// foreman turn's grant only; '' = none chosen, the runner agent's own
	// effort stands.
	ForemanEffort string

	Harness string // the job's harness constraint; '' = any (§8)
	Agent   string // the harness of the agent the lease picked (§8); '' = none
	// AgentAdapter and AgentModel name the picked registration exactly for
	// every job kind; a harness alone can name several agents on a runner.
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
	// ReviewSince is, for a re-review (§7), the head the review it
	// resumes judged: the runner's diff file holds the commits since it.
	// Empty for every other job.
	ReviewSince string
	// LandTarget is, for a land job (§7.1), the branch the task's diff is
	// squashed onto: land_target, or the project's default branch. Empty
	// for every other kind.
	LandTarget string
	// DefaultBranch is, for a land job (§7.1), the project's default
	// branch: the runner creates a land_target the remote does not have
	// yet from it. Empty for every other kind.
	DefaultBranch string
	// LandMessage is, for a land job's squash phase (§7.1), the task
	// data the squash commit message is built from, as the hand-driven
	// land builds it from the API. nil for the push phase and every
	// other kind.
	LandMessage *LandMessage
	// LandCommit is, for a land job's push phase (§7.1), the commit the
	// landing already recorded: the runner pushes exactly it, no new
	// squash. Empty for the squash phase and every other kind.
	LandCommit string
	// Push is true for a land job's push phase (§7.1): the squash and
	// check already happened, and the runner pushes the recorded commit,
	// then runs after_land. False for the squash phase and every other
	// kind.
	Push bool
	// GatePush is the project's push gate a land job reads (§6.3): auto
	// pushes, ask records waiting for the user, never records the exact
	// push command without pushing. Empty for every other kind.
	GatePush string
	// AfterLand is the repo-relative command the runner runs after the
	// push, or after the landing when gate_push is never (§6.3). Empty =
	// none.
	AfterLand string
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
	// SessionGen is the task's session generation (§5.1): how many
	// restarts the task has had at this lease, which names the attempt
	// the job belongs to. Every job of one attempt — its work job, the
	// fix rounds that resume the author's session, its reviews — leases
	// under one generation, and a restart moves it, so the runner can
	// name a worktree path the session's whole round shares: a harness
	// session belongs to the directory it started in. 0 for a foreman
	// turn and a task-less review, which no resume ties to a task.
	SessionGen int64
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
	// A job queued for later (§3, §4.2) is invisible to leasing until its
	// not-before time arrives; the sweep's TTL and the wake's live-job
	// check still see it queued, so it keeps its place in line.
	rows, err := tx.Query(`SELECT `+jobColumns+` FROM jobs
		WHERE status = 'queued' AND (not_before IS NULL OR not_before <= ?) ORDER BY id`, fmtTime(now))
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
		if err := activeProjectTx(tx, j.ProjectID); err != nil {
			return nil, err
		}
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
		if usesKeySession(j) {
			free, err := affinityFreeTx(tx, j.Key, owner)
			if err != nil {
				return nil, err
			}
			if !free {
				continue
			}
		}

		var cands []agentCand
		if j.Kind == KindLand {
			// A land job has no worker and no agent (§7.1): the label
			// check above is the whole placement, and the runner records
			// no agent.
			cands = []agentCand{{}}
		} else {
			cands, err = leaseAgentsTx(tx, &e.cfg, e.cfg.Rand, owner, j, nowStr)
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
				if !becauseplaced && j.Kind == KindReview {
					author, err := reviewAuthorTx(tx, j)
					if err != nil {
						return nil, err
					}
					becauseplaced = author != 0
				}
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
		}
		// The rank choice (§8): the candidates come sampled and ordered by
		// leaseAgentsTx, within the served tier, so a weaker record keeps
		// getting picked now and then and a costly agent less often.
		walk := cands
		// The cap gate (§8.1): the job starts only if every layer on its
		// path has room. The walk takes the first agent in rank order
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
		jobEffort := ""
		if agent != 0 {
			if err := tx.QueryRow("SELECT effort FROM agents WHERE id = ?", agent).Scan(&jobEffort); err != nil {
				return nil, fmt.Errorf("read the effort of agent %d: %w", agent, err)
			}
		}
		effort := ""
		if j.TaskID != 0 && j.Kind != KindLand {
			taskEffort, err := jobEffortTx(tx, e.cfg.Review, j)
			if err != nil {
				return nil, err
			}
			// The task's effective effort (its own, or the escalation
			// ladder's step, §8; a review's own, §7), then the agent's own (already in its
			// harness's vocabulary), then medium: the default is resolved
			// in brigada's levels before the harness maps them, so a
			// harness without that control gets nothing.
			effort = adapter.EffortForHarness(agentHarness, taskEffort)
			if effort == "" {
				effort = jobEffort
			}
			if effort == "" && taskEffort == "" {
				effort = adapter.EffortForHarness(agentHarness, defaultTaskEffort)
			}
			jobEffort = effort
		} else if j.Kind == KindForeman && foremanEffort != "" {
			jobEffort = foremanEffort
		}
		expires := now.Add(e.cfg.LeaseTTL)
		_, err = tx.Exec(`UPDATE jobs SET status = 'leased', lease_id = ?, lease_owner = ?, lease_expires_at = ?,
			lease_epoch = ?, fence_token = attempts + 1, attempts = attempts + 1, agent_id = ?, effort = ?,
			branch = COALESCE(branch, NULLIF(?, '')),
			first_leased = COALESCE(first_leased, ?), last_leased = ?, queued_at = NULL, last_event = NULL,
			cap_held_at = NULL, cap_held_layer = NULL, cap_held_key = NULL, slot_hold = 0,
			released_by = NULL, released_until = NULL, released_lease = NULL, updated_at = ?
			WHERE id = ?`,
			leaseID, owner, fmtTime(expires), epoch, nullIfZero(agent), jobEffort, jobBranch, nowStr, nowStr, nowStr, j.ID)
		if err != nil {
			return nil, fmt.Errorf("lease job %d: %w", j.ID, err)
		}
		usage.Add(cjob.Runner, cjob.Account, cjob.Model, cjob.Plan)
		if usesKeySession(j) {
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
		// A task job's lease stamps the task's session generation (§5.1):
		// its restarts so far, the same count that names the attempt's
		// branch (jobBranchTx), so every job of one attempt — the work
		// job, the fix rounds resuming its session, its reviews — runs
		// under one generation and a restart moves it. Stamped at lease,
		// never at submit: the generation is a property of the attempt
		// the lease joins, and a queued job of the old attempt that a
		// restart replaced never leases again.
		sessionGen := int64(0)
		if j.TaskID != 0 {
			if err := tx.QueryRow("SELECT COUNT(*) FROM jobs WHERE task_id = ? AND restart = 1", j.TaskID).
				Scan(&sessionGen); err != nil {
				return nil, fmt.Errorf("count the restarts of task %d: %w", j.TaskID, err)
			}
			if _, err := tx.Exec("UPDATE jobs SET session_gen = ? WHERE id = ?", sessionGen, j.ID); err != nil {
				return nil, fmt.Errorf("stamp the session generation of job %d: %w", j.ID, err)
			}
		}
		if _, err := appendEventTx(tx, evJobLeased, j.ProjectID, j.TaskID, j.ID,
			eventPayload{Lease: leasePrefix(leaseID), Owner: owner, Attempt: j.Attempts + 1}, now); err != nil {
			return nil, err
		}
		// Why this agent (§8): plain log, no inbox row and no wake. The
		// agents ranked above the pick were passed over for cap room and
		// are named, and every candidate's weight and draw is recorded.
		if agent != 0 && j.Kind != KindForeman {
			weight := chosen.weight
			payload := eventPayload{Agent: agent, Score: chosen.score, Pick: pickPinned, Weight: &weight,
				TierAsked: chosen.tierAsked, TierServed: chosen.tierServed}
			if j.Kind == KindReview && j.Mode == ModeSession {
				payload.Pick = pickResumed // a re-review's agent is the previous review's (§7)
			}
			if j.RequireAgent == 0 {
				payload.Pick, payload.Draw = pickSampled, chosen.draw
				payload.Candidates = make([]Ranked, 0, len(cands))
				for _, c := range cands {
					if c.rank < chosen.rank {
						payload.PassedOver = append(payload.PassedOver, c.id)
					}
					payload.Candidates = append(payload.Candidates, Ranked{Agent: c.id, Weight: c.weight, Draw: c.draw})
				}
			}
			// The picked agent may be the one a review retry's exclusion
			// named: no other qualifying reviewer was available, so the
			// preference was lifted (§7) and the excluded agent runs —
			// history should say why.
			if j.Kind == KindReview && j.ExcludeAgent != 0 && agent == j.ExcludeAgent {
				payload.ExclusionLifted = true
			}
			if _, err := appendEventTx(tx, evJobAgentPicked, j.ProjectID, j.TaskID, j.ID, payload, now); err != nil {
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
		if j.Kind == KindLand {
			// A land grant carries no worker, agent or session; the target
			// branch, the task's result branch and the project's default
			// check are all the runner needs (§7.1).
			return e.landLeaseGrantTx(tx, j, owner, leaseID, expires)
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
		resume, reviewSince := "", ""
		if j.Kind == KindReview && j.Mode == ModeSession && j.TaskID != 0 {
			// A re-review (§7) resumes the session of the review it answers,
			// not the key's, which is the author's; the runner shows it the
			// fix round alone, since the head that review judged.
			if resume, reviewSince, err = reReviewLeaseTx(tx, j); err != nil {
				return nil, err
			}
		} else if j.Mode == ModeSession {
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
		agentAdapter, agentModel := "", ""
		if agent != 0 {
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

			Effort:          effort,
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
			ReviewSince:     reviewSince,
			Branch:          jobBranch,
			Attempt:         j.Attempts + 1,
			ResumeSessionID: resume,
			SessionGen:      sessionGen,
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

// The rank choices a lease records (§8): the agent came from the sampled,
// cost-weighted ranking, or the job named it (a fix round, a pinned restart)
// and nothing was ranked.
const (
	pickSampled = "sampled"
	pickPinned  = "pinned"
)

// defaultTaskEffort is the effort a task job runs at when neither the task
// nor its agent names one, in brigada's task vocabulary (§8).
const defaultTaskEffort = "medium"

// agentCand is one eligible agent of the lease's ranking (§8): its
// harness rides beside the id so the grant can name it, its score is the
// outcome record's for the job's (repo, class), and rank is its place in
// the ranking, 0 the first to try.
type agentCand struct {
	id       int64
	adapter  string // the adapter the agent was registered under: with the model, its identity on its runner
	harness  string
	score    float64
	rank     int
	attempts int
	success  int
	account  string
	weight   int     // the configured cost weight of the agent's (harness, model)
	draw     float64 // p, the sampled success rate the ranking used; 0 when unsampled
	tie      float64 // the further draw that breaks exact ties

	tierAsked  string
	tierServed string

	// Filled for every lookup, but read only for the agents that cannot
	// take the job yet (qualifyingAgentsTx): model, health, cooling_until.
	model   string
	health  string
	cooling string
}

// rankAgents orders the candidates for a pick (§8). Each draws p from
// Beta(successes + 0.5, failures + 0.5), its outcome record for the job's
// (repo, class); they rank by weight / p, lowest first. A weight of 0
// ranks before every positive weight, and among zero weights the higher p
// goes first. Exact ties break by a further draw, so no agent wins them by
// id alone. A nil rnd leaves the candidates in id order, undrawn.
func rankAgents(cands []agentCand, rnd func() float64) {
	if rnd != nil {
		for i := range cands {
			c := &cands[i]
			c.draw = betaDraw(rnd, float64(c.success)+0.5, float64(c.attempts-c.success)+0.5)
			c.tie = rnd()
		}
		sort.SliceStable(cands, func(x, y int) bool { return ranksBefore(&cands[x], &cands[y]) })
	}
	for i := range cands {
		cands[i].rank = i
	}
}

// ranksBefore is rankAgents' order.
func ranksBefore(a, b *agentCand) bool {
	switch {
	case (a.weight == 0) != (b.weight == 0):
		return a.weight == 0
	case a.weight == 0 && a.draw != b.draw:
		return a.draw > b.draw
	case a.weight != 0:
		if ka, kb := float64(a.weight)/a.draw, float64(b.weight)/b.draw; ka != kb {
			return ka < kb
		}
	}
	return a.tie < b.tie
}

// leaseAgentsTx ranks the agents the lease may pick from (§8, §8.1): the
// runner's agents matching the job's harness constraint, ready and out
// of cooling at now, offering the job kind's class, and passing the
// family rule's exclusions when it carries them (§7, §8) — sampled by
// rankAgents from the outcome record of the job's (repo, class) and the
// configured cost weights. A job that requires an agent (§3) — a
// fix round resuming the author's session — has its list named
// outright: that agent or none, never ranked against another. Empty
// when the runner advertises no such agent — the caller then refuses
// the lease, unless the runner advertises nothing at all.
func leaseAgentsTx(tx *sql.Tx, cfg *Config, rnd func() float64, runner int64, j *jobRow, now string) ([]agentCand, error) {
	return eligibleAgentsTx(tx, cfg, rnd, runner, j, kindClass(j.Kind), now)
}

// eligibleAgentsTx shares eligibility and ranking with automatic retries,
// whose candidate lookup uses the task's class rather than the job kind's.
// Runner 0 ranks all registered agents for an automatic restart's pin. A
// caller that only asks who qualifies passes a nil rnd and gets id order.
func eligibleAgentsTx(tx *sql.Tx, cfg *Config, rnd func() float64, runner int64, j *jobRow, class, now string) ([]agentCand, error) {
	return qualifyingAgentsTx(tx, cfg, rnd, runner, j, class, now, false)
}

// qualifyingAgentsTx is eligibleAgentsTx's lookup. With unavailable
// true it also returns the agents that are broken or cooling at now —
// those that qualify for the job by pin, harness, class and family rule
// but cannot take it yet — with their model, health and cooling
// deadline filled in: what the cooling hold (§8.1) and the queue-TTL
// park reason read. Unpinned task jobs are restricted to the served tier
// before availability is checked: the task's effective tier for a work or
// fix job, the review tier for a review (jobTierTx). A review retry's agent exclusion is
// applied as the preference it is (§7): the lift is decided inside
// qualifyingAgentRowsTx, so the tier choice above, the candidates and
// the hold all read the same exclusion.
func qualifyingAgentsTx(tx *sql.Tx, cfg *Config, rnd func() float64, runner int64, j *jobRow, class, now string, unavailable bool) ([]agentCand, error) {
	limits := cfg.Caps
	asked, served := "", ""
	if j.TaskID != 0 && j.Kind != KindForeman {
		var err error
		if asked, err = jobTierTx(tx, cfg.Review, j); err != nil {
			return nil, err
		}
		if j.RequireAgent == 0 {
			served, err = tierServedTx(tx, limits, j, class, asked, now)
			if err != nil {
				return nil, err
			}
		}
	}
	rows, err := qualifyingAgentRowsTx(tx, runner, j, class, now)
	if err != nil {
		return nil, err
	}
	var cands []agentCand
	for _, c := range rows {
		tier := limits.TierOf(c.harness, c.model)
		if served != "" && tier != served {
			continue
		}
		if !unavailable && (c.health != "ready" || c.cooling > now) {
			continue
		}
		if asked != "" {
			c.tierAsked, c.tierServed = asked, tier
		}
		c.weight = limits.CostOf(c.harness, c.model)
		cands = append(cands, c)
	}
	if j.RequireAgent != 0 {
		rnd = nil // a pinned job names its agent: nothing to rank or draw
	}
	rankAgents(cands, rnd)
	return cands, nil
}

// longCooling is how far off a cooling agent's reset may be and still count
// as available for its tier (§8): a window of a few hours is worth waiting
// out, a limit of days is not.
const longCooling = time.Hour

// tierServedTx considers every runner's structurally qualifying agents.
// A short cooling (ending within longCooling of now) still counts as ready: a
// cheap wait beats a heavier model. A longer one counts like a broken agent.
// A model or account capped at exactly 0 can never take a job, so its agents
// do not count as ready; a cap that is merely full still waits (§8). Other
// caps are applied later, after the tier is fixed. When no tier has an
// available agent but some agents are only cooling, the tier of the agent
// that resets first is served, so the job waits for the earliest reset.
func tierServedTx(tx *sql.Tx, limits caps.Limits, j *jobRow, class, asked, now string) (string, error) {
	agents, err := qualifyingAgentRowsTx(tx, 0, j, class, now)
	if err != nil {
		return "", err
	}
	horizon := now
	if t, err := parseTime(now); err == nil {
		horizon = fmtTime(t.Add(longCooling))
	}
	hasReady := map[string]bool{}
	var soonest string
	soonestTiers := map[string]bool{}
	for _, a := range agents {
		if a.health != "ready" || cappedAtZero(limits, a.account, a.model) {
			continue
		}
		tier := limits.TierOf(a.harness, a.model)
		if a.cooling <= horizon {
			hasReady[tier] = true
			continue
		}
		switch {
		case soonest == "" || a.cooling < soonest:
			soonest, soonestTiers = a.cooling, map[string]bool{tier: true}
		case a.cooling == soonest:
			soonestTiers[tier] = true
		}
	}
	if len(hasReady) == 0 {
		hasReady = soonestTiers
	}
	return tierServed(asked, hasReady), nil
}

// cappedAtZero reports whether the model's cap or the account's cap is
// configured as exactly 0 (§8.1).
func cappedAtZero(limits caps.Limits, account, model string) bool {
	if n, ok := limits.Model[model]; ok && n == 0 {
		return true
	}
	n, ok := limits.Account[account]
	return ok && n == 0
}

// tierServed prefers the asked tier, then climbs, then steps down from
// heavy. hasReady names the tiers with an agent that is ready, not capped at
// 0 and not cooling for long. With none anywhere, retain the asked tier.
func tierServed(asked string, hasReady map[string]bool) string {
	tiers := []string{"light", "standard", "heavy"}
	at := slices.Index(tiers, asked)
	if at < 0 || hasReady[asked] {
		return asked
	}
	for _, tier := range tiers[at+1:] {
		if hasReady[tier] {
			return tier
		}
	}
	for i := at - 1; i >= 0; i-- {
		if hasReady[tiers[i]] {
			return tiers[i]
		}
	}
	return asked
}

// qualifyingAgentRowsTx applies the shared pin, harness/model, class,
// family and exclusion filters, without health, cooling or tier. A
// review retry's agent exclusion is a preference (§7, decided
// 2026-10-20): reviewExclusionLiftedTx decides it, so the lease's
// candidates, the tier choice and the structural reviewer checks all
// read the same exclusion. Structural reviewer checks also use this:
// broken or cooling reviewers still exist.
func qualifyingAgentRowsTx(tx *sql.Tx, runner int64, j *jobRow, class, now string) ([]agentCand, error) {
	author, err := reviewAuthorTx(tx, j)
	if err != nil {
		return nil, err
	}
	excludeAgent := j.ExcludeAgent
	if excludeAgent != 0 && j.Kind == KindReview {
		lifted, err := reviewExclusionLiftedTx(tx, j, class, now)
		if err != nil {
			return nil, err
		}
		if lifted {
			excludeAgent = 0
		}
	}
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
	query := `SELECT a.id, a.family, a.harness, a.classes, a.model, a.health, COALESCE(a.cooling_until, ''), COALESCE(o.attempts, 0), COALESCE(o.successes, 0), a.adapter, a.account
		FROM agents a LEFT JOIN outcomes o ON o.agent_id = a.id AND o.project_id = ? AND o.class = ?
		WHERE 1 = 1`
	args := []any{j.ProjectID, class}
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
		var family, harness, classes, model, health, cooling, adapterName, account string
		if err := rows.Scan(&id, &family, &harness, &classes, &model, &health, &cooling, &attempts, &successes, &adapterName, &account); err != nil {
			return nil, fmt.Errorf("pick the agent of runner %d: %w", runner, err)
		}
		// Ordinary work and fix leases use the implement class. A pinned
		// work restart already qualified against the task's class when
		// the retry was queued; do not reject that pin for lacking implement.
		pinnedRetry := j.Kind == KindWork && j.Restart && j.RequireAgent != 0
		if class != "" && j.Kind != KindForeman && !pinnedRetry && !agentOffersClass(classes, class) {
			continue
		}
		if id == excludeAgent {
			continue
		}
		if j.ExcludeFamily != "" || j.Kind == KindReview {
			if !reviewAgentQualifies(id, family, classes, j.ExcludeFamily, excludeAgent, author) {
				continue
			}
		}
		cands = append(cands, agentCand{id: id, adapter: adapterName, harness: harness, model: model, health: health, cooling: cooling,
			account: account, attempts: attempts, success: successes, score: Score(attempts, successes)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pick the agent of runner %d: %w", runner, err)
	}
	for i := range cands {
		cands[i].rank = i
	}
	return cands, nil
}

// kindClass is the dispatch class for ordinary leases (§8): work and fix
// use implement regardless of the task's class; review and foreman jobs
// use review and answer respectively for the outcomes join. Foreman turns
// are exempt from the advertised-class filter: their pair is the constraint.
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
// when a family is excluded, the agent needs a known different family;
// it must also offer review and differ from the author and excluded agent.
// The exclusions name the author, its family and, on a retry, the first
// reviewer's agent; empty values exclude nothing.
func reviewAgentQualifies(id int64, family, classes, excludeFamily string, excludeAgent, author int64) bool {
	switch {
	case id == excludeAgent || id == author:
		return false
	case excludeFamily != "" && (family == "" || family == excludeFamily):
		return false
	}
	return agentOffersClass(classes, string(KindReview))
}

// reviewExclusionLiftedTx reports whether a review job's agent exclusion
// gives way (§7, decided 2026-10-20): the exclusion of the first
// reviewer is a preference, and it holds only while another qualifying
// reviewer is available — ready, or cooling for no more than longCooling
// — so the review never waits days behind another family's reset while
// the excluded agent could run again. With none available the excluded
// agent qualifies again. A now of "" asks structurally: another
// qualifying reviewer of any health keeps the exclusion, the answer the
// queue-time and unservable checks read. The family rule never lifts:
// the author and its family stay closed whatever is available, and only
// the retry exclusion is dropped to look for the other reviewer.
func reviewExclusionLiftedTx(tx *sql.Tx, j *jobRow, class, now string) (bool, error) {
	if j.Kind != KindReview || j.ExcludeAgent == 0 {
		return false, nil
	}
	sans := *j
	sans.ExcludeAgent = 0 // the other reviewers alone; no recursion: the lift no longer applies
	rows, err := qualifyingAgentRowsTx(tx, 0, &sans, class, "")
	if err != nil {
		return false, err
	}
	horizon := now
	if now != "" {
		if t, err := parseTime(now); err == nil {
			horizon = fmtTime(t.Add(longCooling))
		}
	}
	for _, a := range rows {
		if a.id == j.ExcludeAgent {
			continue
		}
		if now != "" && (a.health != "ready" || a.cooling > horizon) {
			continue // broken, or cooling past longCooling: not available
		}
		return false, nil // another qualifying reviewer: the exclusion holds
	}
	return true, nil
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
