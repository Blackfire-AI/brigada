package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Blackfire-AI/brigada/internal/planfile"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// The plan file (§6.4): the engine mirrors each plan as
// .brigada/plans/<plan-id>.yaml on a branch of the project's remote, and
// pushes it after every plan change. The store stays authoritative; the
// file is a mirror that may lag.
//
// What the mirror owes is kept in the store, not in memory: triggers
// (migration 0021) mark a plan in plan_mirror_dirty on every write to the
// plan, its tasks or its edges, so no engine path has to remember. After
// each committed call that left a mark, a background pass renders every
// marked plan, pushes it and clears the mark. The push is best-effort and
// outside the store transaction: a failure (no credentials, no network,
// no default branch yet) is logged as a warning, the mark stays, and the
// next change pushes again — the failed plan's file and any other that
// fell behind with it. A mirror failure never fails a plan.

// defaultPlanFileBranch is the branch the files go to unless the project's
// plan_file_branch says otherwise: not the default branch, because the
// engine never commits there on its own.
const defaultPlanFileBranch = "brigada/plans"

// Shipped defaults of the mirror's pacing.
const (
	defaultPlanFileRetry   = 30 * time.Second // between attempts after a failed push
	defaultPlanFileTimeout = 2 * time.Minute  // for one plan's fetch, commit and push
)

// planFileSettings is the project setting the mirror reads from the
// projects.settings JSON (the pattern of the approval settings, §6.3):
//
//	plan_file_branch   the branch the plan files are pushed to. Absent or
//	                   empty = brigada/plans. The literal "main", or the
//	                   repo's default branch name, commits to the default
//	                   branch instead (§6.4's option); the engine never
//	                   creates that branch, so a remote without it is a
//	                   warning until the repo has one.
//	plan_files         on (default) or off. Off writes and pushes
//	                   nothing: a plan's mark stays owed, so turning it
//	                   on again catches the plans up.
type planFileSettings struct {
	Branch string `json:"plan_file_branch"`
	Files  string `json:"plan_files"`
}

// planFileTarget resolves the setting against the project's repo.
func planFileTarget(repoURL, defaultBranch, setting string) planfile.Target {
	branch := strings.TrimSpace(setting)
	if branch == "" {
		branch = defaultPlanFileBranch
	}
	if branch == "main" || branch == defaultBranch {
		return planfile.Target{URL: repoURL, Branch: defaultBranch, Default: true}
	}
	return planfile.Target{URL: repoURL, Branch: branch}
}

// planMirror is the engine's background push state: at most one pass runs
// at a time, a change during a pass asks for another, and a failed pass
// holds the next attempt back for the retry interval.
type planMirror struct {
	mu      sync.Mutex
	running bool
	again   bool
	retryAt time.Time // real time: it paces git, not the store's clock
	wg      sync.WaitGroup
}

// mirroredStore is the engine's store: every Update is the store's, and
// once its transaction commits, a call that left the mirror owing a push
// starts the pass. Every engine call goes through it, so no path has to
// remember the mirror.
type mirroredStore struct {
	*store.Store
	e *Engine
}

func (m mirroredStore) Update(ctx context.Context, fn func(tx *sql.Tx) error) error {
	e := m.e
	if !e.cfg.PlanFiles {
		return m.Store.Update(ctx, fn)
	}
	owed := false
	err := m.Store.Update(ctx, func(tx *sql.Tx) error {
		owed = false
		if err := fn(tx); err != nil {
			return err
		}
		return tx.QueryRow("SELECT EXISTS (SELECT 1 FROM plan_mirror_dirty)").Scan(&owed)
	})
	if err == nil && owed {
		e.SyncPlanFiles()
	}
	return err
}

// SyncPlanFiles starts a background push of every plan whose file is
// behind the store; a no-op when the mirror is off. The engine does it
// after each call that changed a plan, and a starting engine once, to
// catch up on what an earlier run left behind.
func (e *Engine) SyncPlanFiles() {
	if !e.cfg.PlanFiles {
		return
	}
	m := &e.mirror
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		m.again = true
		return
	}
	if time.Now().Before(m.retryAt) {
		return
	}
	m.running = true
	m.wg.Add(1)
	go e.mirrorPlans()
}

// WaitPlanFiles blocks until the running push pass, if any, has ended: a
// stopping engine calls it before closing the store, and a test before it
// reads the remote.
func (e *Engine) WaitPlanFiles() {
	e.mirror.wg.Wait()
}

func (e *Engine) mirrorPlans() {
	m := &e.mirror
	defer m.wg.Done()
	for {
		failed := e.pushDirtyPlans()
		m.mu.Lock()
		if failed {
			m.retryAt = time.Now().Add(e.cfg.PlanFileRetry)
			m.again = false
		}
		if !m.again {
			m.running = false
			m.mu.Unlock()
			return
		}
		m.again = false
		m.mu.Unlock()
	}
}

// pushDirtyPlans pushes every marked plan and reports whether any push
// failed. A failure is a warning: the mark stays for the next pass.
func (e *Engine) pushDirtyPlans() (failed bool) {
	ctx := context.Background()
	var plans []int64
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT plan_id FROM plan_mirror_dirty ORDER BY plan_id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			plans = append(plans, id)
		}
		return rows.Err()
	})
	if err != nil {
		e.cfg.Warnf("plan files: read the plans owed a push: %v", err)
		return true
	}
	for _, id := range plans {
		if err := e.pushPlanFile(ctx, id); err != nil {
			e.cfg.Warnf("plan files: plan %d not mirrored (the mirror catches up on the next change): %v", id, err)
			failed = true
		}
	}
	return failed
}

// pushPlanFile renders one plan's file from the store and pushes it, then
// clears the plan's mark — unless a change landed meanwhile, which keeps
// it.
func (e *Engine) pushPlanFile(ctx context.Context, planID int64) error {
	var snap planSnapshot
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		var err error
		snap, err = readPlanSnapshotTx(tx, planID)
		return err
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return e.clearPlanMark(ctx, planID, -1) // the plan is gone: nothing to mirror
	case err != nil:
		return err
	}
	if snap.off {
		return nil // the mark stays: turning the mirror on again catches the plan up
	}
	pushCtx, cancel := context.WithTimeout(ctx, e.cfg.PlanFileTimeout)
	defer cancel()
	if err := planfile.Push(pushCtx, snap.target, snap.plan); err != nil {
		return err
	}
	return e.clearPlanMark(ctx, planID, snap.seq)
}

// clearPlanMark drops a plan's mark if it still reads seq (any seq when
// negative).
func (e *Engine) clearPlanMark(ctx context.Context, planID, seq int64) error {
	return e.store.Store.Update(ctx, func(tx *sql.Tx) error { // not the mirroring Update: this write is the mirror's own
		_, err := tx.Exec("DELETE FROM plan_mirror_dirty WHERE plan_id = ? AND (? < 0 OR seq = ?)", planID, seq, seq)
		return err
	})
}

// planSnapshot is a plan as the store holds it at one moment, with where
// its file goes and the mark's seq the render answers.
type planSnapshot struct {
	plan   planfile.Plan
	target planfile.Target
	seq    int64
	off    bool // the project's plan_files is off
}

// readPlanSnapshotTx reads a plan, its tasks, their edges, rounds and open
// questions, in one transaction. sql.ErrNoRows when there is no such plan.
func readPlanSnapshotTx(tx *sql.Tx, planID int64) (planSnapshot, error) {
	var s planSnapshot
	var created, updated, settings, defaultBranch, transport string
	err := tx.QueryRow(`SELECT p.title, p.state, p.created_at, p.updated_at, pr.repo_url, COALESCE(NULLIF(pr.transport_url, ''), pr.repo_url), pr.default_branch, pr.settings
		FROM plans p JOIN projects pr ON pr.id = p.project_id WHERE p.id = ?`, planID).
		Scan(&s.plan.Title, &s.plan.State, &created, &updated, &s.plan.Repo, &transport, &defaultBranch, &settings)
	if err != nil {
		return planSnapshot{}, err
	}
	s.plan.ID = planID
	if s.plan.Created, err = parseTime(created); err != nil {
		return planSnapshot{}, fmt.Errorf("plan %d: created_at: %w", planID, err)
	}
	if s.plan.Updated, err = parseTime(updated); err != nil {
		return planSnapshot{}, fmt.Errorf("plan %d: updated_at: %w", planID, err)
	}
	var cfg planFileSettings
	if settings != "" {
		if err := json.Unmarshal([]byte(settings), &cfg); err != nil {
			return planSnapshot{}, fmt.Errorf("parse the settings of the project of plan %d: %w", planID, err)
		}
	}
	s.target = planFileTarget(transport, defaultBranch, cfg.Branch)
	s.off = cfg.Files == PlanFilesOff
	if err := tx.QueryRow("SELECT COALESCE((SELECT seq FROM plan_mirror_dirty WHERE plan_id = ?), 0)", planID).Scan(&s.seq); err != nil {
		return planSnapshot{}, fmt.Errorf("read the mirror mark of plan %d: %w", planID, err)
	}

	// Rounds are counted as the cap counts them (roundCapReached): the fix
	// jobs that spent a round since the task's latest restart. The open
	// question is the latest task_needs_help of a task still parked, with
	// no answer or restart after it.
	rows, err := tx.Query(`SELECT t.id, t.name, t.title, t.class, t.state, COALESCE(t.branch, ''),
		(SELECT COUNT(*) FROM jobs j WHERE j.task_id = t.id AND j.kind = 'fix' AND j.round IN (?, ?)
			AND j.id > COALESCE((SELECT MAX(r.id) FROM jobs r WHERE r.task_id = t.id AND r.restart = 1), 0)),
		COALESCE((SELECT json_extract(ev.payload, '$.question') FROM events ev
			WHERE ev.task_id = t.id AND ev.kind = ? AND t.state IN (?, ?)
			AND ev.id > COALESCE((SELECT MAX(a.id) FROM events a WHERE a.task_id = t.id AND a.kind IN (?, ?)), 0)
			ORDER BY ev.id DESC LIMIT 1), ''),
		COALESCE(t.extra_repos, ''), t.tier, t.effort, t.review_tier, t.review_effort, t.caps, t.skills
		FROM tasks t WHERE t.plan_id = ? ORDER BY t.id`,
		roundFix, roundReview, evTaskNeedsHelp, taskNeedsForeman, taskNeedsUser, evTaskAnswered, evTaskRestarted, planID)
	if err != nil {
		return planSnapshot{}, fmt.Errorf("read the tasks of plan %d: %w", planID, err)
	}
	index := map[int64]int{}
	for rows.Next() {
		var t planfile.Task
		var extraRec, capsJSON, skills string
		if err := rows.Scan(&t.ID, &t.Name, &t.Title, &t.Class, &t.State, &t.Branch, &t.Rounds, &t.Question, &extraRec, &t.Tier, &t.Effort, &t.ReviewTier, &t.ReviewEffort, &capsJSON, &skills); err != nil {
			rows.Close()
			return planSnapshot{}, fmt.Errorf("read the tasks of plan %d: %w", planID, err)
		}
		t.Caps = parseTaskCaps(capsJSON)
		t.Skills = taskSkillNames(skills)
		// The file carries the identity and ref, never the fetch URL (§5.8).
		if spec := parseExtraReposRecord(extraRec); spec != nil {
			for _, x := range spec.Repos {
				t.ExtraRepos = append(t.ExtraRepos, planfile.ExtraRepo{Name: x.Name, Repo: x.Repo, Ref: x.Ref})
			}
		}
		index[t.ID] = len(s.plan.Tasks)
		s.plan.Tasks = append(s.plan.Tasks, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return planSnapshot{}, fmt.Errorf("read the tasks of plan %d: %w", planID, err)
	}
	rows.Close()

	edges, err := tx.Query(`SELECT d.task_id, x.name, d.is_base, d.is_continue FROM task_deps d JOIN tasks x ON x.id = d.depends_on_task_id
		WHERE d.plan_id = ? ORDER BY d.task_id, x.id`, planID)
	if err != nil {
		return planSnapshot{}, fmt.Errorf("read the edges of plan %d: %w", planID, err)
	}
	defer edges.Close()
	for edges.Next() {
		var task int64
		var dep string
		var isBase, isContinue bool
		if err := edges.Scan(&task, &dep, &isBase, &isContinue); err != nil {
			return planSnapshot{}, fmt.Errorf("read the edges of plan %d: %w", planID, err)
		}
		if i, ok := index[task]; ok {
			s.plan.Tasks[i].Deps = append(s.plan.Tasks[i].Deps, dep)
			if isBase {
				s.plan.Tasks[i].Base = dep
			}
			if isContinue {
				s.plan.Tasks[i].Continue = dep
			}
		}
	}
	if err := edges.Err(); err != nil {
		return planSnapshot{}, fmt.Errorf("read the edges of plan %d: %w", planID, err)
	}
	// The file shows the base the engine will use: the named one, else
	// the only dependency, else the continued dep (§6.1).
	for i, t := range s.plan.Tasks {
		if t.Base == "" {
			switch {
			case len(t.Deps) == 1:
				s.plan.Tasks[i].Base = t.Deps[0]
			case t.Continue != "":
				s.plan.Tasks[i].Base = t.Continue
			}
		}
	}
	return s, nil
}
