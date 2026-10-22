package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// jobRun is one leased job in the runner's hands (§5.1): the workspace,
// the worker, the heartbeats, and the completion the engine hears. It
// runs at most one worker at a time; the runner's loop is sequential.
type jobRun struct {
	cfg    *Config
	client *api.Client
	grant  *api.JobGrant

	mu           sync.Mutex
	events       []adapter.Event
	transcript   []byte
	lastProgress time.Time
	lastActivity time.Time
	toolOpenedAt time.Time
	sessionID    string
	cliError     *adapter.Event
	// stderrError is the first error the adapter sorted from stderr,
	// kept apart from the stream's own cliError: stderr also carries
	// the CLI's warnings, so finish weighs it against the exit, not
	// against the run.
	stderrError  *adapter.Event
	toolInFlight int
	startedAt    time.Time
	workerLive   bool
	dropped      bool
	stopped      *stop
	// The loop watch (§5.6) beside the activity clock: what the tool
	// calls were, so a busy loop is told from progress. loopTrip holds
	// the repeated call's line once the watch trips, and rides every
	// heartbeat from then on; the run itself is not stopped — the
	// foreman decides.
	loop     loopWatch
	loopTrip string

	// extraEnv names the job's extra checkouts (§5.8), one
	// BRIGADA_REPO_<NAME>=<path> each; extraCommits is the commit each
	// resolved to, by name. extraReadDirs holds the same checkouts'
	// absolute paths, in the grant's order, for the adapter's read grant
	// (adapter.Job.ReadDirs); extraPrompt is the fixed paragraph that
	// names them in the job's prompt, so a worker with no shell learns
	// the paths. All are set once, before the setup runs.
	extraEnv      []string
	extraCommits  map[string]string
	extraReadDirs []string
	extraPrompt   string

	// transcriptCap bounds the transcript bytes kept in memory and
	// uploaded; 0 keeps the 64 MiB default — the engine's upload cap's
	// own default (§9). A field, not the constant, so a test can
	// shrink it without writing 64 MiB first.
	transcriptCap int

	// killWorker kills the worker's process group without ending the
	// run: the stall and cap paths use it, because a stopped worker's
	// job still completes — with an outcome — instead of being dropped.
	// It is assigned once, in execute, before the goroutines that may
	// call it start, so it needs no lock.
	killWorker func()

	// mcpConfig is the harness's MCP config for the run (§12.2), set
	// once — on the foreman path, by foremanMCPConfig — before the
	// worker starts, and cleared by cleanupMCPConfig once the worker is
	// dead, so that cleanup is idempotent however often it runs. Every
	// task job keeps the field empty. Read by runWorker alone, between
	// the two, like killWorker.
	mcpConfig string
	// noResume starts the session fresh whatever the grant says: the
	// foreman turn sets it when the CLI rejected the resume as unknown.
	noResume bool
}

// execute runs the job end to end. Every failure is delivered to the
// engine — a completion, an error completion or a release — never into
// the runner's loop; the only silent ends are the ones the engine
// itself ordered (lost, gone, cancelled, fenced out), where the result
// is dropped and nothing is completed (§12.3), and the runner's own
// shutdown, where the lease lapses and the job is re-dispatched.
//
// The heartbeats span the whole job — worker, push, check, deliver —
// not just the worker: a check can run for minutes, and a lease held
// through it without beats would lapse and turn finished work into a
// lost answer. Everything after the worker runs under runCtx, so a bad
// answer mid-check kills the check's process group too.
func (r *jobRun) execute(ctx context.Context) {
	log := r.cfg.Log
	log.InfoContext(ctx, "job leased", "job_id", r.grant.JobID, "kind", r.grant.Kind, "key", r.grant.Key)
	r.startedAt = r.now() // the class time cap measures the whole run, from the lease

	// A foreman turn has no task worktree, push or check (§4). It reads
	// a checkout under the project's scratch directory, where its
	// harness session keeps running under the same machinery.
	if isForemanTurn(r.grant) {
		r.runForemanTurn(ctx)
		return
	}

	// A land job has no worker, no worktree and no setup (§7.1): the
	// runner's own git, in a clone of the target branch under its state
	// directory.
	if isLandJob(r.grant) {
		r.runLandJob(ctx)
		return
	}

	// The worker's kill switch exists before any goroutine that may
	// fire it: the heartbeat loop (a stall) closes the channel, the
	// worker's killer goroutine receives it — the field is written once,
	// here, ahead of both.
	kill := make(chan struct{})
	r.killWorker = sync.OnceFunc(func() { close(kill) })

	// The lease is kept from the moment it is taken, set-up included: a
	// job that waits behind another's clone of the shared mirror, or for
	// an author's worktree to come down, would otherwise lapse and be
	// leased again while this run still meant to build its worktree. An
	// answer that ends the hold aborts runCtx, and with it the set-up.
	runCtx, abort := context.WithCancel(ctx)
	beatsDone := make(chan struct{})
	go r.heartbeats(runCtx, abort, beatsDone)

	ws, err := prepareWorkspace(runCtx, r.cfg, r.grant)
	if err != nil {
		if r.takeDropped() || runCtx.Err() != nil {
			// The hold ended (or the runner is shutting down) during the
			// set-up: nothing is delivered, as for a run that ends later.
			r.cfg.Log.WarnContext(context.WithoutCancel(ctx), "job set-up ended before the worktree was ready; nothing delivered",
				"job_id", r.grant.JobID, "err", err)
		} else {
			// The environment failed before the worker started — the
			// mirror's fetch or clone, the worktree, the branch it builds
			// on — never the agent (issue #39): the job fails as an
			// infrastructure error, so the engine retries it, capped, and
			// charges no health.
			r.finishWithInfraError(runCtx, err.Error())
		}
		abort()
		<-beatsDone
		return
	}
	r.runJob(runCtx, ws, kill)
	r.cleanupSetup()
	r.cleanupExtras()
	abort()
	<-beatsDone

	// The worktree comes down whatever happened, on a context of its
	// own: at shutdown the runner's ctx is already dead, but a dead
	// worktree left on disk would wedge the next attempt of the job.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	ws.remove(cleanupCtx)
}

// runJob is the job after the workspace exists: the worker, then the
// push, the check and the completion — everything the heartbeats cover.
func (r *jobRun) runJob(ctx context.Context, ws *workspace, kill <-chan struct{}) {
	ad, model, err := r.cfg.agentFor(r.grant)
	if err != nil {
		// The lease named an agent this runner does not offer — a stale
		// advertisement, or an engine that knows another set. The job
		// fails with the reason rather than running on a wrong CLI.
		r.finishWithError(ctx, err.Error())
		return
	}
	// The extra repositories (§5.8) come first: setup, the worker and the
	// check all see them. One the runner cannot fetch is an infrastructure
	// failure, delivered like a failed setup.
	if why := r.prepareExtras(ctx); why != "" {
		if r.gone(ctx, "checking out the extra repositories") {
			return
		}
		r.finishWithSetupError(ctx, why)
		return
	}
	if r.gone(ctx, "checking out the extra repositories") {
		return
	}
	// The environment step (§5.7) runs before the worker, as the runner:
	// the worker never sees it or chooses its command. The stall clock
	// has not started, so only the setup's own timeout bounds it. A review
	// job gets the same environment in its worktree.
	if why := r.runSetup(ctx, ws.dir, true); why != "" {
		if r.gone(ctx, "running the setup") {
			return
		}
		r.finishWithSetupError(ctx, why)
		return
	}
	if r.gone(ctx, "running the setup") {
		return
	}
	exitErr := r.runWorker(ctx, ad, model, ws.dir, kill)
	if r.takeDropped() {
		r.cfg.Log.WarnContext(ctx, "job no longer ours; worker killed and result dropped",
			"job_id", r.grant.JobID, "worktree", ws.dir)
		return
	}
	if ctx.Err() != nil {
		// The runner is shutting down: no push, check or completion on a
		// dead context — the lease lapses and the job runs again.
		r.cfg.Log.WarnContext(context.WithoutCancel(ctx), "runner shutting down mid-job; the lease will lapse",
			"job_id", r.grant.JobID)
		return
	}
	if why := r.takeStopped(); why != nil {
		r.finishStopped(ctx, ws, why)
		return
	}
	if isReviewJob(r.grant) {
		r.finishReview(ctx, ad, exitErr)
		return
	}
	r.finish(ctx, ad, ws, exitErr)
}

// runWorker starts the worker CLI in workdir and reads it to the end:
// the event stream is parsed and recorded, and the process group is
// killed the moment the run is aborted — the engine's answer turned
// bad, the runner's own ctx ending, or a stall — while a clean exit is
// waited out. It returns the command's wait error; what became of the
// run is in the jobRun's fields.
func (r *jobRun) runWorker(ctx context.Context, ad adapter.Adapter, model, workdir string, kill <-chan struct{}) error {
	resume := ""
	if r.grant.Mode == string(engine.ModeSession) && !r.noResume {
		resume = r.grant.ResumeSessionID
	}
	// The extra checkouts (§5.8) are named in the prompt — the worker
	// may have no shell to read their variables with — and granted as
	// read-only dirs to the adapter, whatever its CLI offers for one.
	var tools []string // nil: the project set none, the harness's default scope
	if r.grant.WorkerTools != "" {
		if t, err := adapter.ParseToolScope(r.grant.WorkerTools); err == nil {
			tools = t
		}
	}
	cmd, err := ad.Command(adapter.Job{
		Prompt:    r.grant.Prompt + r.extraPromptOf(),
		Model:     model,
		Review:    isReviewJob(r.grant),
		Effort:    r.cfg.effortFor(r.grant),
		MCPConfig: r.mcpConfig,
		ReadDirs:  r.extraDirs(),
		Tools:     tools,
	}, workdir, resume)
	if err != nil {
		return &noWorkerError{err}
	}
	// The worker never sees the runner's own git credentials (§5.1):
	// whatever variables GitEnv names are stripped from the command's
	// environment, whatever the adapter put there. An unset Env means
	// inherit, so it is materialized first for the same scrub.
	cmd.Env = scrubEnv(cmd.Env, r.cfg.GitEnv)
	// The CLI's working directory is its worktree, and so is its PWD:
	// the runner's own PWD points at the state directory, and a Node CLI
	// that reads PWD before the cwd would work there.
	if cmd.Dir == "" {
		cmd.Dir = workdir
	}
	cmd.Env = withPWD(cmd.Env, cmd.Dir)
	// The extra checkouts' paths (§5.8) ride along, after the scrub.
	cmd.Env = withEnv(cmd.Env, r.extraEnvVars())
	cmd.SysProcAttr = processGroup()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &noWorkerError{err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return &noWorkerError{err}
	}
	if err := cmd.Start(); err != nil {
		return &noWorkerError{err}
	}

	// The activity clock starts at the worker's start — before the
	// worker counts as watched, or a beat landing between the two would
	// read a clock that never started: a worker that never reports
	// anything is measured from its start (§5.6).
	r.markActivity(r.now())
	r.setWorkerUp(true)
	defer r.setWorkerUp(false)
	// The killer turns an abort, a stall or a cap into the worker's
	// death: the CLI is not context-aware, so the group is signalled
	// directly — a term first, the kill on its own schedule (§5.6). It
	// stands down when the command is waited out cleanly.
	workerDone := make(chan struct{})
	killerDone := make(chan struct{})
	go func() {
		defer close(killerDone)
		select {
		case <-ctx.Done():
		case <-kill:
		case <-workerDone:
			return
		}
		terminateGroup(cmd, r.cfg.KillGrace)
	}()

	// Stderr rides the same pipeline as stdout (§5.2): the CLI prints
	// some failures only there — a resume-not-found before any JSON, a
	// limit or an auth message — and the adapter turns them into error
	// events like any other. Both readers finish before the wait, as
	// the pipes need draining when Wait closes them; a scanner that
	// gave up on an over-long line stops reading, so the rest of the
	// stream is drained to Discard — a worker that keeps writing into a
	// full pipe would otherwise block on it until the policy kills it.
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			r.recordEvent(ad, sc.Bytes(), true)
		}
		if err := sc.Err(); err != nil {
			r.cfg.Log.WarnContext(ctx, "reading the worker's stderr failed", "job_id", r.grant.JobID, "err", err)
			io.Copy(io.Discard, stderr)
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		r.recordEvent(ad, scanner.Bytes(), false)
	}
	if err := scanner.Err(); err != nil {
		// An unreadably long line ends the record's view of the stream
		// early; the worker runs on, with its output drained.
		r.cfg.Log.WarnContext(ctx, "reading the worker's stream failed", "job_id", r.grant.JobID, "err", err)
		io.Copy(io.Discard, stdout)
	}
	<-stderrDone
	waitErr := cmd.Wait()
	close(workerDone)
	<-killerDone
	return waitErr
}

// heartbeats reports the runner's hold on the job for as long as the
// run lasts — the worker, the push, the check, the delivery — with the
// worker's progress (§5.6). While the worker runs, every beat first
// consults the stall policy: stalled, or past the class time cap, the
// worker's tree is killed and the run completes with that outcome —
// and the beats go on, because the completion still needs the lease.
// An engine answer that ends the hold — lost, gone, cancelled, fenced
// out — aborts the run: the worker's (or the check's) process group is
// killed and whatever the run produced is dropped, never completed
// (§12.3). A transport failure is not an answer: the beat is logged
// and the next one tried, and if the lease lapses in the meantime a
// later beat answers lost.
func (r *jobRun) heartbeats(ctx context.Context, abort context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	ticker := r.cfg.Clock.NewTicker(r.cfg.HeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
			if r.workerUp() {
				if why := r.stallPolicy().check(r.now(), r.timing()); why != nil {
					// The policy stop kills the worker alone: the run
					// itself goes on to complete the job with the
					// outcome (§5.6), so the run's context stays alive.
					// killWorker was set before this goroutine started;
					// it is never nil here.
					r.markStopped(why)
					r.killWorker()
				}
			}
			ans, _, err := r.client.Heartbeat(ctx, r.grant.JobID, r.grant.LeaseID, r.progress())
			if err != nil {
				if ctx.Err() == nil {
					r.cfg.Log.WarnContext(ctx, "heartbeat failed", "job_id", r.grant.JobID, "err", err)
				}
				continue
			}
			switch ans {
			case engine.AnsOK:
			case engine.AnsLost, engine.AnsGone, engine.AnsCancelled, engine.AnsStaleEpoch:
				r.markDropped()
				r.cfg.Log.WarnContext(ctx, "the engine ended our hold; killing the run's processes",
					"job_id", r.grant.JobID, "answer", ans)
				abort()
				return
			default:
				r.cfg.Log.WarnContext(ctx, "heartbeat answer", "job_id", r.grant.JobID, "answer", ans)
			}
		}
	}
}

// stallPolicy is the run's watch (§5.6): the configured silence window,
// and the caps of the class the job's kind belongs to.
func (r *jobRun) stallPolicy() stallPolicy {
	caps := r.cfg.ClassCaps[r.grant.Kind]
	return stallPolicy{
		StallAfter: r.cfg.StallAfter,
		ToolCap:    caps.ToolCap,
		TimeCap:    caps.TimeCap,
		StartedAt:  r.startedAt,
	}
}

// now is the run's clock — real time in production, the injected one in
// tests (§5.6).
func (r *jobRun) now() time.Time { return r.cfg.Clock.Now() }

// recordEvent parses one line of the worker's output — stdout's event
// stream, or stderr riding the same pipeline (§5.2) — and folds it into
// the run's record: the event list (for the final message and the
// resume id), the transcript (verbatim, for the artifact; stderr lines
// tagged as such), the activity clock and the in-flight tool count
// (§5.6). The transcript stays bounded: past twice the cap it is
// compacted to its head and tail, so a runaway stream neither outgrows
// memory nor prices itself out of the upload.
func (r *jobRun) recordEvent(ad adapter.Adapter, line []byte, fromStderr bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fromStderr {
		r.transcript = append(r.transcript, "stderr: "...)
	}
	r.transcript = append(r.transcript, line...)
	r.transcript = append(r.transcript, '\n')
	if len(r.transcript) > 2*r.uploadCap() {
		r.transcript = compactTranscript(r.transcript, r.uploadCap())
	}
	var ev adapter.Event
	var ok bool
	if fromStderr {
		ev, ok = ad.ParseErrEvent(line)
	} else {
		ev, ok = ad.ParseEvent(line)
	}
	if !ok {
		return
	}
	ev.Time = r.now()
	r.events = append(r.events, ev)
	if ev.SessionID != "" {
		r.sessionID = ev.SessionID
	}
	switch ev.Kind {
	case adapter.EvError:
		// A stderr-sorted error is kept apart from the stream's own
		// (see stderrError): stderr carries warnings beside failures.
		if fromStderr {
			if r.stderrError == nil {
				e := ev
				r.stderrError = &e
			}
		} else if r.cliError == nil {
			e := ev
			r.cliError = &e
		}
	case adapter.EvToolStart:
		if r.toolInFlight == 0 {
			r.toolOpenedAt = ev.Time // the outermost open call bounds the suspension
		}
		r.toolInFlight++
		r.lastProgress = ev.Time
		r.lastActivity = ev.Time
		// The loop watch counts the call (§5.6); a start with no
		// arguments to identify it counts nothing, and the first key to
		// trip the watch is the loop the signal names, whatever follows.
		if key := loopKey(ev); key != "" {
			if n := r.loop.record(loopKeyHash(key), ev.Time); n >= loopCalls && r.loopTrip == "" {
				r.loopTrip = loopCallText(ev)
			}
		}
	case adapter.EvToolEnd:
		if r.toolInFlight > 0 {
			r.toolInFlight--
		}
		r.lastProgress = ev.Time
		r.lastActivity = ev.Time
	case adapter.EvMessage:
		r.lastProgress = ev.Time
		r.lastActivity = ev.Time
	}
}

// progress is the heartbeat's payload (§5.6): when the worker last made
// progress, when it was last active — the worker's start, until its
// first progress event — the last few progress events, and the loop
// signal with the repeated call once the watch has tripped. Times the
// run has not reached yet stay zero, and the client leaves them out: a
// beat before any activity reports a hold, not a fake timestamp.
func (r *jobRun) progress() api.Progress {
	r.mu.Lock()
	defer r.mu.Unlock()
	const keep = 5
	var recent []adapter.Event
	for i := len(r.events) - 1; i >= 0 && len(recent) < keep; i-- {
		if r.events[i].IsProgress() {
			recent = append(recent, r.events[i])
		}
	}
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	return api.Progress{
		LastProgressAt: r.lastProgress,
		LastActivityAt: r.lastActivity,
		Events:         adapter.ProgressEvents(recent),
		Loop:           r.loopTrip != "",
		LoopCall:       r.loopTrip,
	}
}

// snapshotEvents copies the events for reading off the run's mutex.
func (r *jobRun) snapshotEvents() []adapter.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]adapter.Event(nil), r.events...)
}

// recentProgress renders the last n progress events (§5.6) — what a
// stopped run's completion attaches, so the record names what the
// worker was doing, not just that it stopped.
func (r *jobRun) recentProgress(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var recent []adapter.Event
	for i := len(r.events) - 1; i >= 0 && len(recent) < n; i-- {
		if r.events[i].IsProgress() {
			recent = append(recent, r.events[i])
		}
	}
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	return adapter.ProgressEvents(recent)
}

// timing snapshots what the stall policy reads (§5.6): the activity
// clock, and whether — and since when — a tool call is in flight.
func (r *jobRun) timing() runTiming {
	r.mu.Lock()
	defer r.mu.Unlock()
	return runTiming{
		LastActivity: r.lastActivity,
		ToolOpenedAt: r.toolOpenedAt,
		ToolInFlight: r.toolInFlight > 0,
	}
}

// markActivity sets the activity clock, at the worker's start.
func (r *jobRun) markActivity(t time.Time) {
	r.mu.Lock()
	r.lastActivity = t
	r.mu.Unlock()
}

func (r *jobRun) setWorkerUp(up bool) {
	r.mu.Lock()
	r.workerLive = up
	r.mu.Unlock()
}

// workerUp reports whether the worker's process is the run's current
// phase: the stall policy watches the worker (§5.6), not the runner's
// own push and check, which make progress of their own.
func (r *jobRun) workerUp() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.workerLive
}

func (r *jobRun) markDropped() {
	r.mu.Lock()
	r.dropped = true
	r.mu.Unlock()
}

func (r *jobRun) takeDropped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// gone reports whether the run stopped being ours while doing what —
// the engine ended the hold, or the runner is shutting down — and logs
// it. The phases behind the worker (push, check, delivery) consult it
// instead of reporting their own cancellation as their failure.
func (r *jobRun) gone(ctx context.Context, what string) bool {
	if !r.takeDropped() && ctx.Err() == nil {
		return false
	}
	r.cfg.Log.WarnContext(context.WithoutCancel(ctx),
		"run no longer ours; killing it and dropping the result",
		"job_id", r.grant.JobID, "phase", what)
	return true
}

// markStopped records why the runner's policy cut the run short; the
// first verdict stands — a check on a later beat cannot rename a stop
// already delivered to the kill switch.
func (r *jobRun) markStopped(why *stop) {
	r.mu.Lock()
	if r.stopped == nil {
		r.stopped = why
	}
	r.mu.Unlock()
}

func (r *jobRun) takeStopped() *stop {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// stoppedRunEventsKept is how many progress events a stopped run's
// completion attaches (§5.6): enough to see what the worker was doing
// when it went quiet, bounded so a runaway stream cannot swamp the
// record.
const stoppedRunEventsKept = 20

// finishStopped completes a run the runner's own policy cut short
// (§5.6): stalled, or past the class time cap. The outcome ends the
// job as stalled — the engine's answer to "always goes to the foreman"
// — and the result block carries the reason and the last progress
// events, so the record names what the worker was doing when it
// stopped; and the worker's work is not lost — whatever it committed,
// or left uncommitted, goes to the result branch before the worktree
// comes down, exactly as a done run's would. A review is the exception
// (§7): it holds nothing of its own to save, and its branch is the
// work round's, so nothing is pushed for one.
func (r *jobRun) finishStopped(ctx context.Context, ws *workspace, why *stop) {
	res := WorkerResult{
		Status:   why.Outcome,
		Reason:   why.Reason,
		Progress: r.recentProgress(stoppedRunEventsKept),
	}
	if !isReviewJob(r.grant) && ws.holdsWork(ctx) {
		if err := ws.ensurePushed(ctx); err != nil {
			if r.gone(ctx, "pushing the stopped run's branch") {
				return
			}
			r.cfg.Log.WarnContext(ctx, "push the stopped run's branch",
				"job_id", r.grant.JobID, "err", err)
			res.Summary = "the branch could not be pushed: " + err.Error()
		} else {
			res.Branch = ws.branch
		}
	}
	comp := api.Completion{SessionID: r.sessionIDOf(), Outcome: why.Outcome}
	if err := r.saveTranscript(ctx); err != nil {
		r.cfg.Log.WarnContext(ctx, "save transcript", "job_id", r.grant.JobID, "err", err)
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		r.finishWithError(ctx, "encode the result block: "+err.Error())
		return
	}
	comp.Result = string(encoded)
	r.deliver(ctx, comp)
}

// finish turns the dead worker's record into the engine's completion
// (§5.4, §5.5): the result block, the branch push it reported, the
// check the runner ran, and the artifacts. The stream's own error beats
// everything the worker said — a CLI that failed produced no result
// worth reading. An error sorted from stderr counts only against a run
// that did not end clean: stderr carries the CLI's warnings — an MCP
// server that wants attention, a rate-limited retry — and a warning
// beside a good result must not fail the run.
func (r *jobRun) finish(ctx context.Context, ad adapter.Adapter, ws *workspace, exitErr error) {
	var cliErr *adapter.Event
	r.mu.Lock()
	switch {
	case r.cliError != nil:
		e := *r.cliError
		cliErr = &e
	case r.stderrError != nil && exitErr != nil:
		e := *r.stderrError
		cliErr = &e
	}
	r.mu.Unlock()
	if cliErr != nil {
		r.finishWithCLIError(ctx, fmt.Sprintf("worker CLI failed (%s): %s", cliErr.ErrorKind, cliErr.Text), cliErr)
		return
	}
	if nw := (&noWorkerError{}); errors.As(exitErr, &nw) {
		// The CLI never started — a command that could not be built or
		// spawned is the environment's failure, not the agent's (issue
		// #39): the infra kind, retried and charged to no health.
		r.finishWithInfraError(ctx, "start the worker: "+nw.err.Error())
		return
	}

	result := parseResult(ad.ResultText(r.snapshotEvents()))
	if result.Reason == reasonNoResult && exitErr != nil {
		result.Summary = fmt.Sprintf("worker exited with %v; %s", exitErr, result.Summary)
	}

	comp := api.Completion{SessionID: r.sessionIDOf()}
	if err := r.saveTranscript(ctx); err != nil {
		r.cfg.Log.WarnContext(ctx, "save transcript", "job_id", r.grant.JobID, "err", err)
	}

	result.Head = "" // the runner's to say, as the extra repositories' commits are
	result.ChangedPaths = nil
	if result.Status == statusDone {
		if err := ws.ensurePushed(ctx); err != nil {
			if r.gone(ctx, "pushing the branch") {
				return
			}
			r.finishWithError(ctx, err.Error())
			return
		}
		if head, err := ws.worktreeHead(ctx); err == nil {
			result.Head = head
		}
		// The task's changed paths (§6.3, §7.1) are the runner's to say
		// too: recorded so the land gate's protected-path rule matches
		// what the work really touched. A failure to read them is a
		// warning, never the job's — the gate falls back to its proxy.
		if paths, err := ws.changedPaths(ctx, r.grant.BaseRef); err != nil {
			r.cfg.Log.WarnContext(ctx, "record the task's changed paths", "job_id", r.grant.JobID, "err", err)
		} else {
			result.ChangedPaths = paths
		}
		if result.Branch == "" {
			result.Branch = ws.branch
		}
		if r.grant.CheckCmd != "" {
			// The worker may have changed a lock file or a script the
			// setup calls, so it runs again before the check (§5.7); it
			// never replaces the shared cache entry now.
			if why := r.runSetup(ctx, ws.dir, false); why != "" {
				if r.gone(ctx, "running the setup before the check") {
					return
				}
				r.finishWithSetupError(ctx, why)
				return
			}
			if r.gone(ctx, "running the setup before the check") {
				return
			}
			check := runCheck(ctx, ws.dir, r.grant.CheckCmd, r.cfg.CheckTimeout, r.extraEnvVars()...)
			if r.gone(ctx, "running the check") {
				// The check was cut short by the run ending, not by the
				// check: a killed check's exit code says nothing.
				return
			}
			comp.CheckResult = check.JSON()
			if err := r.saveRecord(ctx, "check_log", fmt.Sprintf("job-%d-check.log", r.grant.JobID), []byte(check.Log)); err != nil {
				r.cfg.Log.WarnContext(ctx, "save check log", "job_id", r.grant.JobID, "err", err)
			}
			if check.Failed() {
				// §5.5: a failed check is the job's outcome, not a footnote.
				result.Status = statusCheckFail
			}
		}
	} else if result.Status == statusFailed {
		branch := fmt.Sprintf("%s-salvage-%d", ws.branch, r.grant.JobID)
		saved, err := ws.salvage(ctx, branch)
		if r.gone(ctx, "saving the failed run's edits") {
			return
		}
		var note string
		if err != nil {
			r.cfg.Log.WarnContext(ctx, "salvage the failed run's edits",
				"job_id", r.grant.JobID, "branch", branch, "err", err)
			note = "edits could not be saved on branch " + branch + ": " + err.Error()
		} else if saved {
			note = "edits saved on branch " + branch
		}
		if note != "" {
			if result.Summary != "" {
				result.Summary += "; "
			}
			result.Summary += note
		}
	}
	// The commit each extra repository resolved to is the runner's to say
	// (§5.8); a worker's own claim is replaced.
	result.ExtraRepos = r.extraCommits
	encoded, err := json.Marshal(result)
	if err != nil {
		r.finishWithError(ctx, "encode the result block: "+err.Error())
		return
	}
	comp.Result = string(encoded)
	r.deliver(ctx, comp)
}

// sessionIDOf reads the session id the worker's stream carried.
func (r *jobRun) sessionIDOf() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessionID
}

// finishWithError completes the job with an error: the run cannot
// produce a result, and the reason is the engine's to keep.
func (r *jobRun) finishWithError(ctx context.Context, msg string) {
	r.deliverError(ctx, api.Completion{Error: msg, SessionID: r.sessionIDOf()})
}

// finishWithCLIError is finishWithError for a failure the CLI itself
// reported: the error's kind rides with the completion, and a limit's
// reset time when the adapter found one (§4.4).
func (r *jobRun) finishWithCLIError(ctx context.Context, msg string, cliErr *adapter.Event) {
	r.deliverError(ctx, api.Completion{
		Error:      msg,
		SessionID:  r.sessionIDOf(),
		ErrorKind:  string(cliErr.ErrorKind),
		LimitUntil: cliErr.LimitUntil,
	})
}

// finishWithSetupError completes the job with a failed setup (§5.7): an
// infrastructure failure, sorted as such so the engine retries it as a
// transient error and charges neither a fix round nor the agent. The
// setup's log is already uploaded; the message carries the exit or the
// timeout alone, since the command's output is data, not a message.
func (r *jobRun) finishWithSetupError(ctx context.Context, msg string) {
	r.deliverError(ctx, api.Completion{
		Error:     msg,
		SessionID: r.sessionIDOf(),
		ErrorKind: engine.ErrorKindSetup,
	})
}

// finishWithInfraError completes the job with a failure of its own
// plumbing: a fetch of the repository's remote that could not be done
// (issue #46 — right after a wake the network is not up for a few
// seconds, and the host does not resolve). Sorted as infrastructure, as
// a failed setup is (§5.7), so the engine retries the job as a
// transient error, capped, and neither the agent nor a round is
// charged.
func (r *jobRun) finishWithInfraError(ctx context.Context, msg string) {
	r.deliverError(ctx, api.Completion{
		Error:     msg,
		SessionID: r.sessionIDOf(),
		ErrorKind: engine.ErrorKindInfra,
	})
}

func (r *jobRun) deliverError(ctx context.Context, comp api.Completion) {
	if err := r.saveTranscript(ctx); err != nil {
		r.cfg.Log.WarnContext(ctx, "save transcript", "job_id", r.grant.JobID, "err", err)
	}
	r.deliver(ctx, comp)
}

// deliver completes the job, honouring the engine's answers (§12.3):
// ok and already end the run well; lost, gone, cancelled and
// stale_epoch mean the result is not ours to deliver anymore, and it is
// dropped — never retried into someone else's job.
func (r *jobRun) deliver(ctx context.Context, comp api.Completion) {
	ans, err := r.complete(ctx, comp)
	if err != nil {
		if errors.Is(err, errDropped) {
			r.cfg.Log.WarnContext(context.WithoutCancel(ctx),
				"completion dropped; the run was taken from us mid-delivery",
				"job_id", r.grant.JobID)
			return
		}
		r.cfg.Log.ErrorContext(ctx, "complete", "job_id", r.grant.JobID, "err", err)
		return
	}
	switch ans {
	case engine.AnsOK, engine.AnsAlready:
		r.cfg.Log.InfoContext(ctx, "job complete", "job_id", r.grant.JobID, "answer", ans)
	case engine.AnsLost, engine.AnsGone, engine.AnsCancelled, engine.AnsStaleEpoch:
		r.cfg.Log.WarnContext(ctx, "completion dropped; the job is no longer ours",
			"job_id", r.grant.JobID, "answer", ans)
	default:
		r.cfg.Log.WarnContext(ctx, "complete answer", "job_id", r.grant.JobID, "answer", ans)
	}
}

// errDropped marks a delivery abandoned mid-retry: a heartbeat
// answered lost, gone, cancelled or fenced-out while the complete was
// between attempts, so the lease is not ours to speak about anymore.
var errDropped = errors.New("run dropped during delivery")

// complete delivers the completion, retrying transport failures a few
// times: a result that was earned should not die with one dropped
// connection, but after the retries the lease's lapse and fencing take
// over — that is their job. Between attempts the heartbeats are still
// listening: a bad answer there ends the retries at once, because the
// engine would refuse the completion anyway.
func (r *jobRun) complete(ctx context.Context, comp api.Completion) (engine.Answer, error) {
	comp.Usage = sumUsage(r.snapshotEvents())
	if !r.startedAt.IsZero() {
		seconds := r.now().Sub(r.startedAt).Seconds()
		comp.WallSeconds = &seconds
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if r.takeDropped() {
				return "", errDropped
			}
			select {
			case <-ctx.Done():
				return "", lastErr
			case <-time.After(time.Second):
			}
		}
		ans, err := r.client.Complete(ctx, r.grant.JobID, r.grant.LeaseID, comp)
		if err == nil {
			return ans, nil
		}
		if ctx.Err() != nil {
			return "", err
		}
		lastErr = err
		r.cfg.Log.WarnContext(ctx, "complete failed; retrying", "job_id", r.grant.JobID, "attempt", attempt+1, "err", err)
	}
	return "", lastErr
}

// saveTranscript uploads the run's whole event stream — stdout's events
// and the stderr that rode the same pipeline — as the job's transcript
// artifact (§9).
func (r *jobRun) saveTranscript(ctx context.Context) error {
	r.mu.Lock()
	data := append([]byte(nil), r.transcript...)
	r.mu.Unlock()
	if len(data) == 0 {
		return nil
	}
	return r.saveRecord(ctx, "transcript", fmt.Sprintf("job-%d-transcript.jsonl", r.grant.JobID), data)
}

// saveRecord uploads one run record to the engine as an artifact (§9)
// and keeps a copy under the runner's state directory, where a run's
// own output stays reachable whatever the engine does. A record over
// the artifact cap is compacted — head and tail, the middle dropped —
// before it is written or uploaded, so a runaway stream costs its
// middle, not its record. The upload is fenced like the completion it
// precedes and idempotent by content, so a retry costs nothing.
func (r *jobRun) saveRecord(ctx context.Context, kind, name string, data []byte) error {
	data = compactTranscript(data, r.uploadCap())
	dir := filepath.Join(r.cfg.StateDir, "records")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		return err
	}
	ans, _, err := r.client.UploadArtifact(ctx, r.grant.JobID, r.grant.LeaseID, kind, data)
	if err != nil {
		return err
	}
	switch ans {
	case engine.AnsOK, engine.AnsAlready:
		return nil
	default:
		return fmt.Errorf("the engine answered %s", ans)
	}
}

// noWorkerError marks a failure to build or start the worker command:
// the run never happened, so there is no exit to interpret.
type noWorkerError struct{ err error }

func (e *noWorkerError) Error() string { return e.err.Error() }

func sumUsage(events []adapter.Event) *adapter.Usage {
	var total *adapter.Usage
	for _, ev := range events {
		if ev.Usage == nil {
			continue
		}
		if total == nil {
			total = &adapter.Usage{}
		}
		total.Add(ev.Usage)
	}
	return total
}
