package runner

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	sessionID    string
	cliError     *adapter.Event
	toolInFlight int
	dropped      bool
	stalled      string

	// killWorker kills the worker's process group without ending the
	// run: the stall path uses it, because a stalled worker's job still
	// completes — as an error — instead of being dropped. It is
	// assigned once, in execute, before the goroutines that may call it
	// start, so it needs no lock.
	killWorker func()
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

	// A foreman turn is not a worker job: it has no task, so there is no
	// workspace to make. The headless foreman wake-up arrives with the
	// foreman phase; until a runner can run it, the job goes back with
	// the reason, for whatever runner can.
	if r.grant.TaskID == 0 {
		r.giveBack(ctx, "this runner cannot run foreman turns yet")
		return
	}

	ws, err := prepareWorkspace(ctx, r.cfg, r.grant)
	if err != nil {
		// A task with no repo URL or an unbuildable worktree cannot be
		// run by anyone; the job fails with the reason instead of
		// bouncing between runners.
		r.finishWithError(ctx, err.Error())
		return
	}

	// The worker's kill switch exists before any goroutine that may
	// fire it: the heartbeat loop (a stall) closes the channel, the
	// worker's killer goroutine receives it — the field is written once,
	// here, ahead of both.
	kill := make(chan struct{})
	r.killWorker = sync.OnceFunc(func() { close(kill) })

	runCtx, abort := context.WithCancel(ctx)
	beatsDone := make(chan struct{})
	go r.heartbeats(runCtx, abort, beatsDone)
	r.runJob(runCtx, ws, kill)
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
	ad, model := r.cfg.adapterFor(r.grant)
	exitErr := r.runWorker(ctx, ad, model, ws, kill)
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
	if reason := r.takeStalled(); reason != "" {
		r.finishWithError(ctx, reason)
		return
	}
	r.finish(ctx, ad, ws, exitErr)
}

// runWorker starts the worker CLI and reads it to the end: the event
// stream is parsed and recorded, and the process group is killed the
// moment the run is aborted — the engine's answer turned bad, the
// runner's own ctx ending, or a stall — while a clean exit is waited
// out. It returns the command's wait error; what became of the run is
// in the jobRun's fields.
func (r *jobRun) runWorker(ctx context.Context, ad adapter.Adapter, model string, ws *workspace, kill <-chan struct{}) error {
	resume := ""
	if r.grant.Mode == string(engine.ModeSession) {
		resume = r.grant.ResumeSessionID
	}
	cmd, err := ad.Command(adapter.Job{Prompt: r.grant.Prompt, Model: model}, ws.dir, resume)
	if err != nil {
		return &noWorkerError{err}
	}
	// The worker never sees the runner's own git credentials (§5.1):
	// whatever variables GitEnv names are stripped from the command's
	// environment, whatever the adapter put there. An unset Env means
	// inherit, so it is materialized first for the same scrub.
	cmd.Env = scrubEnv(cmd.Env, r.cfg.GitEnv)
	cmd.SysProcAttr = processGroup()
	cmd.Stderr = io.Discard // the event stream is stdout; adapters parse it
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &noWorkerError{err}
	}
	if err := cmd.Start(); err != nil {
		return &noWorkerError{err}
	}

	r.setLastProgress(time.Now()) // a worker with no events yet is measured from its start
	// The killer turns an abort or a stall into the worker's death: the
	// CLI is not context-aware, so the group is signalled directly. It
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
		killGroup(cmd)
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		r.recordEvent(ad, scanner.Bytes())
	}
	if err := scanner.Err(); err != nil {
		// An unreadably long line ends the stream early; the worker runs
		// on, but the record names what happened to its output.
		r.cfg.Log.WarnContext(ctx, "reading the worker's stream failed", "job_id", r.grant.JobID, "err", err)
	}
	waitErr := cmd.Wait()
	close(workerDone)
	<-killerDone
	return waitErr
}

// heartbeats reports the runner's hold on the job for as long as the
// run lasts — the worker, the push, the check, the delivery — with the
// worker's progress (§5.6). An engine answer that ends the hold —
// lost, gone, cancelled, fenced out — aborts the run: the worker's (or
// the check's) process group is killed and whatever the run produced
// is dropped, never completed (§12.3). A transport failure is not an
// answer: the beat is logged and the next one tried, and if the lease
// lapses in the meantime a later beat answers lost.
func (r *jobRun) heartbeats(ctx context.Context, abort context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(r.cfg.HeartbeatEvery)
	defer ticker.Stop()
	stall := stallPolicy{StallAfter: r.cfg.StallAfter, ToolCap: r.cfg.ToolCap}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if stall.stalled(time.Now(), r.lastProgressOrStart(), r.toolsInFlight() > 0) {
				// A stall kills the worker alone: the run itself goes on
				// to complete the job as an error (§5.6), so the run's
				// context stays alive. killWorker was set before this
				// goroutine started; it is never nil here.
				r.markStalled(fmt.Sprintf("worker stalled: no progress for over %s", r.cfg.StallAfter))
				r.killWorker()
				return
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

// recordEvent parses one stdout line and folds it into the run's
// record: the event list (for the final message and the resume id), the
// transcript (verbatim, for the artifact), the progress clock and the
// in-flight tool count (§5.6).
func (r *jobRun) recordEvent(ad adapter.Adapter, line []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transcript = append(r.transcript, line...)
	r.transcript = append(r.transcript, '\n')
	ev, ok := ad.ParseEvent(line)
	if !ok {
		return
	}
	ev.Time = time.Now()
	r.events = append(r.events, ev)
	switch ev.Kind {
	case adapter.EvSession:
		if ev.SessionID != "" {
			r.sessionID = ev.SessionID
		}
	case adapter.EvError:
		if r.cliError == nil {
			e := ev
			r.cliError = &e
		}
	case adapter.EvToolStart:
		r.toolInFlight++
		r.lastProgress = ev.Time
	case adapter.EvToolEnd:
		if r.toolInFlight > 0 {
			r.toolInFlight--
		}
		r.lastProgress = ev.Time
	case adapter.EvMessage:
		r.lastProgress = ev.Time
	}
}

// progress is the heartbeat's payload (§5.6): when the worker last made
// progress, the last sign of life, and the last few progress events.
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
		LastActivityAt: time.Now(),
		Events:         adapter.ProgressEvents(recent),
	}
}

// snapshotEvents copies the events for reading off the run's mutex.
func (r *jobRun) snapshotEvents() []adapter.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]adapter.Event(nil), r.events...)
}

func (r *jobRun) setLastProgress(t time.Time) {
	r.mu.Lock()
	r.lastProgress = t
	r.mu.Unlock()
}

func (r *jobRun) lastProgressOrStart() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastProgress.IsZero() {
		return time.Now()
	}
	return r.lastProgress
}

func (r *jobRun) toolsInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.toolInFlight
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

func (r *jobRun) markStalled(reason string) {
	r.mu.Lock()
	if r.stalled == "" {
		r.stalled = reason
	}
	r.mu.Unlock()
}

func (r *jobRun) takeStalled() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stalled
}

// finish turns the dead worker's record into the engine's completion
// (§5.4, §5.5): the result block, the branch push it reported, the
// check the runner ran, and the artifacts. The CLI's own error beats
// everything the worker said — a CLI that failed produced no result
// worth reading.
func (r *jobRun) finish(ctx context.Context, ad adapter.Adapter, ws *workspace, exitErr error) {
	var cliErr *adapter.Event
	r.mu.Lock()
	if r.cliError != nil {
		e := *r.cliError
		cliErr = &e
	}
	r.mu.Unlock()
	if cliErr != nil {
		r.finishWithError(ctx, fmt.Sprintf("worker CLI failed (%s): %s", cliErr.ErrorKind, cliErr.Text))
		return
	}
	if nw := (&noWorkerError{}); errors.As(exitErr, &nw) {
		r.finishWithError(ctx, "start the worker: "+nw.err.Error())
		return
	}

	result := parseResult(ad.ResultText(r.snapshotEvents()))
	if result.Reason == reasonNoResult && exitErr != nil {
		result.Summary = fmt.Sprintf("worker exited with %v; %s", exitErr, result.Summary)
	}

	comp := api.Completion{SessionID: r.sessionIDOf()}
	if err := r.saveTranscript(&comp); err != nil {
		r.cfg.Log.WarnContext(ctx, "save transcript", "job_id", r.grant.JobID, "err", err)
	}

	if result.Status == statusDone {
		if err := ws.ensurePushed(ctx); err != nil {
			if r.gone(ctx, "pushing the branch") {
				return
			}
			r.finishWithError(ctx, err.Error())
			return
		}
		if result.Branch == "" {
			result.Branch = ws.branch
		}
		if r.grant.CheckCmd != "" {
			check := runCheck(ctx, ws.dir, r.grant.CheckCmd, r.cfg.CheckTimeout)
			if r.gone(ctx, "running the check") {
				// The check was cut short by the run ending, not by the
				// check: a killed check's exit code says nothing.
				return
			}
			comp.CheckResult = check.JSON()
			r.saveRecord(&comp, "check_log", fmt.Sprintf("job-%d-check.log", r.grant.JobID), []byte(check.Log))
			if check.Failed() {
				// §5.5: a failed check is the job's outcome, not a footnote.
				result.Status = statusCheckFail
			}
		}
	}
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
	comp := api.Completion{Error: msg, SessionID: r.sessionIDOf()}
	if err := r.saveTranscript(&comp); err != nil {
		r.cfg.Log.WarnContext(ctx, "save transcript", "job_id", r.grant.JobID, "err", err)
	}
	r.deliver(ctx, comp)
}

// giveBack releases the job for the next runner, with the reason kept.
func (r *jobRun) giveBack(ctx context.Context, reason string) {
	ans, err := r.client.Release(ctx, r.grant.JobID, r.grant.LeaseID, reason)
	if err != nil {
		r.cfg.Log.ErrorContext(ctx, "release", "job_id", r.grant.JobID, "err", err)
		return
	}
	r.cfg.Log.InfoContext(ctx, "job released", "job_id", r.grant.JobID, "answer", ans, "reason", reason)
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

// saveTranscript writes the run's event stream to the runner's records
// and references it as the job's transcript artifact (§9).
func (r *jobRun) saveTranscript(comp *api.Completion) error {
	r.mu.Lock()
	data := append([]byte(nil), r.transcript...)
	r.mu.Unlock()
	if len(data) == 0 {
		return nil
	}
	return r.saveRecord(comp, "transcript", fmt.Sprintf("job-%d-transcript.jsonl", r.grant.JobID), data)
}

// saveRecord writes one run record under the runner's state directory
// and attaches it to the completion as an artifact.
func (r *jobRun) saveRecord(comp *api.Completion, kind, name string, data []byte) error {
	dir := filepath.Join(r.cfg.StateDir, "records")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	comp.Artifacts = append(comp.Artifacts, api.Artifact{
		Kind:   kind,
		Path:   path,
		Size:   int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]),
	})
	return nil
}

// noWorkerError marks a failure to build or start the worker command:
// the run never happened, so there is no exit to interpret.
type noWorkerError struct{ err error }

func (e *noWorkerError) Error() string { return e.err.Error() }
