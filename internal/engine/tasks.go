package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// TaskParams is one task submitted whole (§12.1: tasks, create): the
// debug submit path until the foreman's plan tools exist — one repo, one
// base ref, the spec verbatim, and the check the runner runs after a
// done (§5.5). The worker prompt is not the caller's to bring: the
// engine composes it from the spec (§1.4, §5.3).
type TaskParams struct {
	RepoURL  string
	BaseRef  string
	Title    string
	Spec     string
	CheckCmd string // '' = no check
	Class    string // '' = implement
	// Harness and Labels are the queued job's placement constraints
	// (§8): the harness that must run it and the labels a runner must
	// offer in full.
	Harness string
	Labels  []string
	// Skills names, in order, the project's imported skills the task's
	// prompts carry (§5.10): resolved and copied onto the task once, at
	// creation; nil or empty = none.
	Skills []string
}

// TaskResult is what SubmitTask did: the task it created, the project it
// found or made for the repo, and the job submit's own answer.
type TaskResult struct {
	Answer    Answer
	ProjectID int64
	TaskID    int64
	JobID     int64
	Run       int
}

// SubmitTask creates one task and queues its first work job (§12.1, the
// debug path until the foreman exists): the project is found by repo URL
// or created, the task lands ready on a plan of its own, and the job —
// its prompt composed by the engine from the versioned template plus the
// spec verbatim (§1.4, §5.3) — is submitted on the task's key in the
// same transaction, so a caller never sees a task whose job failed to
// appear.
func (e *Engine) SubmitTask(ctx context.Context, p TaskParams) (TaskResult, error) {
	var res TaskResult
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		r, err := e.submitTaskTx(tx, p, now)
		if err != nil {
			return fmt.Errorf("submit task for %s: %w", RedactRepoURL(p.RepoURL), err)
		}
		res = r
		return nil
	})
	return res, err
}

func (e *Engine) submitTaskTx(tx *sql.Tx, p TaskParams, now time.Time) (TaskResult, error) {
	switch {
	case p.RepoURL == "":
		return TaskResult{}, fmt.Errorf("repo URL is required (product §5 rule 6: repos are addressed by URL)")
	case p.BaseRef == "":
		return TaskResult{}, fmt.Errorf("base ref is required: the ref the worktree starts from")
	case CheckBaseFetchable(p.BaseRef) != nil:
		return TaskResult{}, CheckBaseFetchable(p.BaseRef)
	case p.Spec == "":
		return TaskResult{}, fmt.Errorf("spec is required: the worker's task, verbatim (§1.4)")
	}
	class := p.Class
	if class == "" {
		class = "implement"
	}
	title := p.Title
	if title == "" {
		title = taskTitleOf(p.Spec)
	}
	nowStr := fmtTime(now)

	// The project is the repo (§4.1: one project per repository): find
	// it by URL, else create it with the base ref's branch as its
	// default. The foreman's harness and model are setup's to choose;
	// the debug path leaves them empty.
	parts, err := parseRepo(p.RepoURL)
	if err != nil {
		return TaskResult{}, err
	}
	var project int64
	var projectTransport string
	err = lookupProjectTx(tx, p.RepoURL, "id, COALESCE(NULLIF(transport_url, ''), repo_url)", &project, &projectTransport)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		project, err = createProjectTx(tx, p.RepoURL, baseBranchOf(p.BaseRef), "", "", now)
		if err != nil {
			return TaskResult{}, err
		}
		projectTransport = parts.Transport
	default:
		return TaskResult{}, fmt.Errorf("read the project of %s: %w", RedactRepoURL(p.RepoURL), err)
	}
	if err := activeProjectTx(tx, project); err != nil {
		return TaskResult{}, err
	}
	// The runner fetches with the URL the task named (§5.1): when it
	// is another form of the project's repo than the project holds, the
	// task keeps its own credential-free transport.
	taskRepo := ""
	if parts.Transport != "" && parts.Transport != projectTransport {
		taskRepo = parts.Transport
	}

	// One plan per submitted task: the debug path has no plan builder,
	// so each task is a plan of its own, active from the start (§6.2).
	plan, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
		VALUES (?, ?, 'active', ?, ?)`, project, title, nowStr, nowStr)
	if err != nil {
		return TaskResult{}, fmt.Errorf("create the task's plan: %w", err)
	}
	planID, err := plan.LastInsertId()
	if err != nil {
		return TaskResult{}, fmt.Errorf("create the task's plan: last insert id: %w", err)
	}

	// The task itself (§6.1): ready at once, on the project's repo — a
	// task's own repo_url stays NULL unless it was named by another
	// transport than the project holds; taskInfoTx reads NULL as "the
	// project's" (§6.1).
	setup, err := taskProjectSetupTx(tx, project)
	if err != nil {
		return TaskResult{}, err
	}
	// The extra repositories (§5.8): the debug submit names none of its
	// own, so the task takes the project default.
	extraDef, err := projectExtraReposTx(tx, project)
	if err != nil {
		return TaskResult{}, err
	}
	extraRec := extraReposRecord(extraDef.spec(nil))
	task, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, repo_url, setup, extra_repos, created_at, updated_at)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, 'ready', ?, '{}', NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		planID, title, p.Spec, p.CheckCmd, class, p.BaseRef, taskRepo, setup, extraRec, nowStr, nowStr)
	if err != nil {
		return TaskResult{}, fmt.Errorf("create the task: %w", err)
	}
	taskID, err := task.LastInsertId()
	if err != nil {
		return TaskResult{}, fmt.Errorf("create the task: last insert id: %w", err)
	}
	// What the task's prompts carry resolves here, once (§5.9, §5.10):
	// both role texts and the named skills copied onto the task, so a
	// later change to a setting, a role text or the skills import never
	// alters it, and the first prompt below carries the recorded
	// snapshot: the worker text and the worker-audience skills.
	ti, err := resolveTaskInstructionsTx(tx, project, title, p.Skills)
	if err != nil {
		return TaskResult{}, err
	}
	if err := recordTaskInstructionsTx(tx, taskID, ti, now); err != nil {
		return TaskResult{}, err
	}

	sr, err := e.submitTx(tx, SubmitParams{
		Key:        fmt.Sprintf("task:%d", taskID),
		ProjectID:  project,
		TaskID:     taskID,
		Kind:       KindWork,
		Mode:       ModeFresh,
		Prompt:     prompt.ComposeWorkerPrompt(p.Spec, p.CheckCmd, "", workerGuidance(ti)...),
		PromptMeta: marshalPromptMeta(workerPromptMeta(ti)),
		Harness:    p.Harness,
		Labels:     p.Labels,
	}, now)
	if err != nil {
		return TaskResult{}, err
	}
	return TaskResult{
		Answer:    sr.Answer,
		ProjectID: project,
		TaskID:    taskID,
		JobID:     sr.JobID,
		Run:       sr.Run,
	}, nil
}

// taskTitleOf derives a task's title from its spec: the first non-empty
// line, cut to a headline's length.
func taskTitleOf(spec string) string {
	for _, line := range strings.Split(spec, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const max = 80
		if len(line) > max {
			line = line[:max] + "…"
		}
		return line
	}
	return "a task"
}

// baseBranchOf turns a base ref into a project's default branch: refs'
// heads keep their short name, anything else stays as it is.
func baseBranchOf(ref string) string {
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return b
	}
	return ref
}
