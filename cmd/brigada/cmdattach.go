package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/adapter/codex"
	"github.com/Blackfire-AI/brigada/internal/adapter/kimi"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
	"github.com/Blackfire-AI/brigada/internal/runner"
)

// runAttach is the client's attach (§4.2 item 1): the user's own chat
// with the foreman, and what a bare `brigada` runs. The project is the
// one the working directory's git origin names — the way every caller
// names a project by its repo URL (§12.1) — the attach takes the
// foreman's key (waiting out a live turn, or cancelling it with
// --force), and the attached session runs on the terminal until the
// harness exits, when the hold is released with the session id the
// chat ended on.
func runAttach(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("attach",
		"brigada attach [--force] [--no-start] [--model <model>] [--effort <level>]",
		"Attach to this repository’s foreman and chat.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	force := fs.Bool("force", false, "cancel a live foreman turn rather than wait for it to finish")
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	model := fs.String("model", "", "model for this chat only, instead of the project's foreman model")
	effort := fs.String("effort", "", "reasoning effort for this chat only, instead of the project's foreman effort")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "attach takes no positional arguments, got %q", extra)
	}
	dir, err := os.Getwd()
	if err != nil {
		return fail(stderr, fmt.Errorf("find the working directory: %w", err))
	}
	repo, err := gitOriginURL(dir)
	if err != nil {
		// The directory maps to no project: say which projects there are,
		// when a running engine can be asked (it is never started for this).
		if rt, code := resolveRuntime(common, io.Discard); code == 0 {
			if client, code := connect(ctx, rt, "client", true, io.Discard); code == 0 {
				if hint := knownProjectsHint(ctx, client); hint != "" {
					err = fmt.Errorf("%w; %s", err, hint)
				}
			}
		}
		return fail(stderr, err)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	// The project and its foreman, resolved before the key is taken: a
	// directory that is no project's repo, or a project with no foreman,
	// is the caller's own situation, named plainly before anything is
	// held (§4.1).
	setting, err := client.ProjectForeman(ctx, repo)
	if err != nil {
		if isNotFound(err) {
			msg := fmt.Sprintf(
				"no project for repo %s (the origin of %s); choose its foreman first: brigada project foreman --repo %s --harness <harness> --model <model>",
				repo, dir, repo)
			if hint := knownProjectsHint(ctx, client); hint != "" {
				msg += "; " + hint
			}
			return fail(stderr, errors.New(msg))
		}
		return fail(stderr, err)
	}
	if setting.Harness == "" || setting.Model == "" {
		return fail(stderr, fmt.Errorf(
			"the project of %s has no foreman; choose one: brigada project foreman --repo %s --harness <harness> --model <model>",
			repo, repo))
	}
	// The overrides are this chat's alone: the session is the same, and
	// the next headless turn runs on the project's own setting again.
	if err := adapter.ValidateEffort(setting.Harness, *effort); err != nil {
		return usagef(stderr, "%v", err)
	}
	res, code := attachWithWait(ctx, client, repo, *force, stdout, stderr)
	if code != 0 {
		return code
	}
	if *model != "" {
		res.Model = *model
	}
	if *effort != "" {
		res.Effort = *effort
	}
	return runAttachedSession(ctx, rt, client, setting.ProjectID, res, stdout, stderr)
}

// attachBusyRetry is how long the client waits between attach attempts
// while the foreman runs a headless turn (§4.2 item 1): often enough to
// notice a finished turn without pestering the engine. A var so the
// tests can shorten it.
var attachBusyRetry = 3 * time.Second

// attachWithWait takes the foreman's key (§4.2 item 1), waiting out a
// live turn: the client says the foreman is busy and retries until the
// turn frees the key. A second live attach is not waited out — another
// terminal holds the session, and only its user ends that.
func attachWithWait(ctx context.Context, client *api.Client, repo string, force bool, stdout, stderr io.Writer) (api.AttachResult, int) {
	for {
		res, err := client.Attach(ctx, repo, force)
		if err != nil {
			return res, fail(stderr, err)
		}
		if res.Answer != engine.AnsBusy {
			return res, 0
		}
		if res.JobID != 0 {
			fmt.Fprintf(stdout, "the foreman is busy with a turn (job %d); waiting…\n", res.JobID)
			select {
			case <-ctx.Done():
				return res, fail(stderr, fmt.Errorf("attach interrupted while job %d held the foreman; retry when it ends, or --force", res.JobID))
			case <-time.After(attachBusyRetry):
			}
			continue
		}
		if res.InteractiveHoldID != 0 {
			return res, fail(stderr, fmt.Errorf(
				"your own harness session holds the foreman (interactive hold %d); it yields or lapses on its own, or release it: brigada hold release --repo %s",
				res.InteractiveHoldID, repo))
		}
		return res, fail(stderr, fmt.Errorf(
			"the foreman is already attached elsewhere (attach hold %d is live); one attached chat at a time",
			res.HoldID))
	}
}

// adapterForAttach names the adapter that drives a foreman harness's
// CLI for an attached chat (§4.2 item 1). A var so the tests can offer
// the fake adapter; the shipped set is the harnesses whose CLIs
// brigada drives.
var adapterForAttach = defaultAdapterForAttach

// defaultAdapterForAttach is the shipped set: the harnesses the client
// can drive, each of which still has to implement adapter.Interactive
// to carry an attached chat.
func defaultAdapterForAttach(harness string) (adapter.Adapter, bool) {
	switch harness {
	case "claude":
		return &claude.Adapter{}, true
	case "codex":
		// Codex can drive an interactive attach, but remains excluded from
		// ResumeCapableHarnesses until its exec path can carry the foreman's
		// §12.2 MCP bridge. Keep the adapter registered so attach is ready
		// when that foreman gate is revisited.
		return &codex.Adapter{}, true
	case "kimi":
		return &kimi.Adapter{}, true
	}
	return nil, false
}

// runAttachedSession runs the attached chat itself (§4.2 item 1): the
// MCP config written for the hold's token, the new session id the
// client generates so it knows what to write back, the harness
// interactive on the terminal's own streams with the terminal's
// interrupts forwarded to it, the hold heartbeated while the chat
// lives, and — whatever ends it — the config deleted and the hold
// released with the id the chat ended on.
func runAttachedSession(ctx context.Context, rt *runtime, client *api.Client, project int64, res api.AttachResult, stdout, stderr io.Writer) int {
	// A failure from here on holds a hold the client cannot use: it is
	// released on the way out — with the session id the attach found,
	// for the session never moved — so the foreman is not left held by
	// a client that never chatted (§4.2 item 1).
	abandon := func(err error) int {
		releaseAttach(ctx, client, res.HoldID, res.SessionID, stderr)
		return fail(stderr, err)
	}
	ad, ok := adapterForAttach(res.Harness)
	if !ok {
		return abandon(fmt.Errorf("no adapter on this machine drives the %s CLI the foreman runs on; the foreman's harness must be one brigada can run", res.Harness))
	}
	inter, ok := ad.(adapter.Interactive)
	if !ok {
		return abandon(fmt.Errorf("the %s CLI cannot be attached to: it offers no interactive session to resume; switch the foreman to a harness that does (brigada project foreman)", res.Harness))
	}
	stateDir := runnerStateDir(rt)
	// The same reading checkout the runner's foreman turns use (§4):
	// the harness keys its sessions by working directory, so the attach
	// must run exactly there to be the same session — the one rule
	// ForemanScratchDir and PrepareForemanCheckout share. The checkout
	// is also what makes the chat feel like the user's own: the repo's
	// CLAUDE.md, .claude/ and project memory load as in any clone.
	scratch, err := runner.ForemanScratchDir(stateDir, project)
	if err != nil {
		return abandon(fmt.Errorf("make the foreman's scratch directory: %w", err))
	}
	checkout, err := runner.PrepareForemanCheckout(ctx, scratch, res.RepoURL, res.DefaultBranch, nil)
	if err != nil {
		return abandon(fmt.Errorf("prepare the foreman checkout: %w", err))
	}
	res.Prompt = foreman.WithCheckout(res.Prompt, checkout)
	// The MCP config for the hold's token (§12.2): the bridge is this
	// own binary, the token reaches the file and the bridge alone, and
	// the file dies with the attach, however the attach ends.
	config, err := runner.WriteMCPConfig(stateDir, rt.paths.Socket, res.ForemanToken)
	if err != nil {
		return abandon(fmt.Errorf("write the attach's MCP config: %w", err))
	}
	removeConfig := func() {
		if err := runner.RemoveMCPConfig(config); err != nil {
			fmt.Fprintf(stderr, "brigada: warning: remove the attach's MCP config %s: %v (the token it holds dies with the hold)\n", config, err)
		}
	}
	newSessionID, err := newSessionID()
	if err != nil {
		defer removeConfig()
		return abandon(fmt.Errorf("generate the new session id: %w", err))
	}
	job := adapter.Job{
		Prompt: res.Prompt, Model: res.Model, Effort: res.Effort, MCPConfig: config,
		StatusLine: statusLineCommand(rt),
	}
	// build makes the harness command resuming the given session, or
	// starting fresh for "". The terminal's own streams, not the
	// command's writers: the attached session is the user's, and in
	// production the two are the same thing — the attach just never
	// routes the harness through a buffer that could hold its output
	// back. The one exception is a resume's stderr, copied to a small
	// tail as well: it is where the CLI says it does not know the
	// session, which the attach then starts fresh instead.
	build := func(resume string) (*exec.Cmd, *tailBuffer, error) {
		cmd, err := inter.AttachCommand(job, checkout.Path, resume, newSessionID)
		if err != nil {
			return nil, nil, err
		}
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		var tail *tailBuffer
		if resume != "" {
			tail = &tailBuffer{}
			cmd.Stderr = io.MultiWriter(os.Stderr, tail)
		}
		return cmd, tail, nil
	}
	cmd, tail, err := build(res.SessionID)
	if err != nil {
		defer removeConfig()
		return abandon(fmt.Errorf("build the %s session: %w", res.Harness, err))
	}
	// The interrupt trap is armed before the harness starts, so no
	// signal can slip past it: from here to the harness's exit, the
	// terminal's SIGINT and SIGTERM are this session's to forward.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	fmt.Fprintf(stdout, "attached to the foreman of project %d (hold %d, session %s)\n", project, res.HoldID, newSessionID)
	if err := cmd.Start(); err != nil {
		defer removeConfig()
		return abandon(fmt.Errorf("start the %s session: %w", res.Harness, err))
	}
	// Whatever ends the session from here — a clean exit, a crash, a
	// signal — the config dies with it and the hold is released with
	// the id the chat ended on, which is the client's own whatever the
	// CLI did (§4.2: the client generated it for exactly this).
	defer removeConfig()
	defer releaseAttach(ctx, client, res.HoldID, newSessionID, stderr)

	// The session's own context is the terminal's with the interrupts
	// taken out (main wraps them in signal.NotifyContext, so a Ctrl-C
	// cancels it): the chat does not end when the terminal interrupts
	// the harness — that is the user speaking to it — so neither the
	// beats nor anything else below rides the canceled context. The
	// release above already stands on its own the same way.
	sessionCtx := context.WithoutCancel(ctx)
	beatCtx, stopBeats := context.WithCancel(sessionCtx)
	beatsDone := make(chan struct{})
	go func() {
		defer close(beatsDone)
		attachHeartbeats(beatCtx, client, res.HoldID, stderr)
	}()
	// The terminal's interrupts go to the harness, exactly as received
	// and nothing else: a Ctrl-C is the user speaking to the foreman's
	// CLI — whose TUI reads SIGINT as "interrupt the turn", not "die" —
	// and the client stays alive to release the hold after it, however
	// many interrupts the chat takes. Deriving any part of this from
	// ctx.Done would turn every Ctrl-C into a SIGTERM behind the
	// forwarded SIGINT's back; the goroutine therefore ends with the
	// session alone.
	forwardDone := make(chan struct{})
	stopForward := sync.OnceFunc(func() { close(forwardDone) })
	var current atomic.Pointer[exec.Cmd]
	current.Store(cmd)
	go func() {
		for {
			select {
			case sig := <-sigs:
				if c := current.Load(); c != nil && c.Process != nil {
					_ = c.Process.Signal(sig)
				}
			case <-forwardDone:
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	if waitErr != nil && tail != nil && resumeRejected(ad, tail.String()) {
		// The CLI does not know the session the key recorded (its own
		// files lost, or made in another working directory): the
		// resume was a saving, not a requirement, so the chat starts a
		// fresh session, and the release writes its id back as usual.
		fmt.Fprintf(stderr, "brigada: the %s CLI does not know the foreman's last session (%s); starting a fresh one\n", res.Harness, res.SessionID)
		fresh, _, err := build("")
		if err == nil {
			err = fresh.Start()
		}
		if err != nil {
			waitErr = fmt.Errorf("start a fresh %s session: %w", res.Harness, err)
		} else {
			current.Store(fresh)
			waitErr = fresh.Wait()
		}
	}
	stopForward()
	stopBeats()
	<-beatsDone
	if waitErr == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) && ee.ExitCode() >= 0 {
		return ee.ExitCode()
	}
	if errors.As(waitErr, &ee) {
		// The harness died of a signal — the forwarding above, or the
		// terminal's own — which is a detach, not a failure to report.
		return 0
	}
	return fail(stderr, waitErr)
}

// tailBuffer keeps the last bytes written to it: the end of a resumed
// session's stderr, where a CLI that does not know the session says so.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

// tailKept bounds what a tailBuffer keeps.
const tailKept = 8 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailKept {
		t.buf = t.buf[len(t.buf)-tailKept:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// resumeRejected reports whether the CLI's stderr says it does not know
// the session it was asked to resume: any line the adapter sorts as a
// resume failure (the claude CLI's "No conversation found" class).
func resumeRejected(ad adapter.Adapter, stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		if ev, ok := ad.ParseErrEvent([]byte(line)); ok && ev.ErrorKind == adapter.ErrResumeFailed {
			return true
		}
	}
	return false
}

// attachBeatEvery is how often the client heartbeats its attach hold
// (§4.2 item 1): a third of the hold's life, so a lost beat never
// lapses it.
var attachBeatEvery = 30 * time.Second

// attachBeatFailuresBeforeWarn is how many consecutive heartbeats may
// fail to reach the engine before the client says so (§4.2 item 1):
// three misses ≈ the hold's whole TTL, which is the moment the hold
// starts lapsing rather than blinking.
const attachBeatFailuresBeforeWarn = 3

// attachHeartbeats extends the attach hold while the chat lives (§4.2
// item 1). A transport failure is retried by the next beat — the socket
// blinks — but a streak of them is the engine being gone, and after
// enough of one the user hears it: the hold is on its own now, and the
// foreman may be woken for a headless turn while the terminal still
// shows the session. A hold the engine no longer has says so at once
// and ends the beats.
func attachHeartbeats(ctx context.Context, client *api.Client, holdID int64, stderr io.Writer) {
	ticker := time.NewTicker(attachBeatEvery)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		ans, err := client.AttachHeartbeat(ctx, holdID)
		if err != nil {
			failures++
			if failures == attachBeatFailuresBeforeWarn {
				fmt.Fprintf(stderr, "brigada: warning: the engine has not answered the last %d heartbeats; the attach hold may lapse while you chat — the foreman could be woken headless under you\n", failures)
			}
			continue
		}
		failures = 0
		switch ans {
		case engine.AnsOK:
		case engine.AnsGone, engine.AnsExpired:
			fmt.Fprintf(stderr, "brigada: warning: the attach hold is gone; the foreman may be woken for a headless turn while you are still attached — detach when you can\n")
			return
		}
	}
}

// releaseAttach ends the attach hold (§4.2 item 1) with the session id
// the chat ended on, whatever ended it. The release outlives the
// canceled context it may ride in on — a Ctrl-C that ended the chat
// must not kill the write-back with it — and a failure is said aloud
// with what to do: the hold expires on its own, but the id it never
// wrote back means the next turn resumes an older conversation.
func releaseAttach(ctx context.Context, client *api.Client, holdID int64, sessionID string, stderr io.Writer) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, err := client.ReleaseAttach(rctx, holdID, sessionID); err != nil {
		fmt.Fprintf(stderr, "brigada: releasing the attach hold %d failed: %v\n"+
			"the hold expires on its own within a few minutes; the session id %s was not written back, so the next turn resumes an older conversation\n",
			holdID, err, sessionID)
	}
}

// gitOriginURL reads the origin remote's URL of the repository at dir:
// the name a project goes by everywhere (§12.1), and the attach's only
// way to find which foreman the working directory belongs to.
func gitOriginURL(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(errb.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("the working directory %s has no origin URL to name its project by (%s); attach runs inside a project's repository", dir, detail)
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", fmt.Errorf("the origin remote of %s carries no URL; attach names the project by the repo's origin URL", dir)
	}
	return url, nil
}

// newSessionID generates the harness session id for an attached chat
// (§4.2 item 1): a random UUID, minted by the client so it knows the
// id the chat forks onto — the CLI names it --session-id, and the
// release writes it back — without reading the CLI's own session
// files.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4, random
	b[8] = (b[8] & 0x3f) | 0x80 // the RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// statusLineCommand is the shell command the attached session's status
// bar runs (§10): this very binary's `status --line` on the attach's own
// state directory and config, so the bar reads the engine the foreman
// works with. The paths are made absolute first: the bar runs in the
// session's working directory, the foreman's checkout, not where the
// attach was started. Empty — no status bar — when the binary cannot
// name its own path.
func statusLineCommand(rt *runtime) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	parts := []string{shellQuote(exe), "status", "--line", "--state-dir", shellQuote(absPath(rt.stateDir))}
	if rt.configPath != "" {
		parts = append(parts, "--config", shellQuote(absPath(rt.configPath)))
	}
	return strings.Join(parts, " ")
}

// absPath is p made absolute against the current directory, or p itself
// when the current directory cannot be read.
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// shellQuote quotes s for a POSIX shell: single quotes around it, each
// single quote inside closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
