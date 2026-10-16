package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/runner"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The attach tests run the client command against a real engine started
// in-process (§13), the way the status and runner tests do, with the
// foreman pointed at the fake adapter: the attached session is a script
// that records its argv, its environment, its opening prompt and its
// working directory, then ends when the test releases it.

// attachEnv is one test's engine, store and seeded project.
type attachEnv struct {
	stateDir string
	socket   string
	st       *store.Store
	project  int64
}

// newAttachEnv starts the engine on a fresh state dir and seeds one
// project for repo whose foreman runs the given harness, keeping a
// store handle the test reads and seeds through.
func newAttachEnv(t *testing.T, repo, harness, model string) *attachEnv {
	t.Helper()
	state := testsupport.ShortStateDir(t)
	st, err := store.Open(context.Background(), lifecycle.PathsOf(state).Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Format(engine.TimeLayout)
	var project int64
	err = st.Update(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES (?, 'main', '{}', ?, ?, ?, ?)`, repo, harness, model, now, now)
		if err != nil {
			return err
		}
		project, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("seed the project: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the engine stopped with an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the engine did not stop")
		}
	})
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &attachEnv{stateDir: state, socket: socket, st: st, project: project}
}

// scratch is the project's foreman scratch directory (§4) — the runner's
// own rule through the shared helper, <state>/runner/foreman/<project>.
func (e *attachEnv) scratch(t *testing.T) string {
	t.Helper()
	dir, err := runner.ForemanScratchDir(filepath.Join(e.stateDir, "runner"), e.project)
	if err != nil {
		t.Fatalf("make the scratch dir: %v", err)
	}
	return dir
}

// scratchFile reads one file the attached session left in the scratch
// directory, waiting for it to appear: the session's records are the
// test's window into what ran.
func (e *attachEnv) scratchFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(e.scratch(t), name)
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
		if time.Now().After(deadline) {
			t.Fatalf("read %s from the scratch directory: %v", name, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// seedForemanJob plants a live foreman turn for the project (§4.2 item
// 2), the busy an attach waits out: a queued job on the foreman key.
func (e *attachEnv) seedForemanJob(t *testing.T) int64 {
	t.Helper()
	now := time.Now().UTC().Format(engine.TimeLayout)
	var job int64
	err := e.st.Update(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO jobs (project_id, kind, key, mode, prompt, status, created_at, updated_at)
			VALUES (?, 'foreman', ?, 'session', 'a wake', 'queued', ?, ?)`,
			e.project, fmt.Sprintf("foreman:%d", e.project), now, now)
		if err != nil {
			return err
		}
		job, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("seed the foreman turn: %v", err)
	}
	return job
}

// queryRow1 runs a one-column query the test reads state with.
func (e *attachEnv) queryRow1(t *testing.T, query string, args ...any) string {
	t.Helper()
	var out string
	if err := e.st.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(query, args...).Scan(&out)
	}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// latestHoldState reads the newest attach hold's state.
func (e *attachEnv) latestHoldState(t *testing.T) string {
	t.Helper()
	return e.queryRow1(t, "SELECT state FROM attach_holds WHERE id = (SELECT MAX(id) FROM attach_holds)")
}

// adminClient is an API client with the admin token, for the calls the
// test drives by hand (a cancel, above all).
func (e *attachEnv) adminClient(t *testing.T) *api.Client {
	t.Helper()
	_, secret, err := lifecycle.TokenForRole(lifecycle.PathsOf(e.stateDir).Tokens, "admin")
	if err != nil {
		t.Fatalf("read the admin token: %v", err)
	}
	return api.NewClient(e.socket, secret)
}

// markerDir makes the directory the scripted sessions wait on and
// points the environment's BRIGADA_TEST_MARKERS at it, so the test
// holds a session open and releases it when it chooses.
func markerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BRIGADA_TEST_MARKERS", dir)
	return dir
}

// writeAttachScript writes the scripted attached session: it records
// its argv, its environment, its opening prompt and its working
// directory in the scratch directory, then ends when the release
// marker appears. Every record is written whole under a temp name and
// renamed into place — a rename in one directory is atomic, so a
// record's existence means its content is complete, and the test's
// polling read can never catch a half-written file (the shell's `>`
// creates the file empty before the write fills it).
func writeAttachScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "attach.sh")
	body := `#!/bin/sh
printf '%s\n' "$@" > argv.tmp
pwd > where.tmp
env > env.tmp
printf '%s' "$BRIGADA_FAKE_PROMPT" > prompt.tmp
mv argv.tmp argv.txt
mv where.tmp where.txt
mv env.tmp env.txt
mv prompt.tmp prompt.txt
while [ ! -f "$BRIGADA_TEST_MARKERS/release" ]; do sleep 0.05; done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write the attach script: %v", err)
	}
	return script
}

// offerFakeAdapter points the attach at the fake adapter for the fake
// harness, restoring the shipped set when the test ends.
func offerFakeAdapter(t *testing.T, script string) {
	t.Helper()
	orig := adapterForAttach
	adapterForAttach = func(harness string) (adapter.Adapter, bool) {
		if harness == "fake" {
			return &fake.Adapter{Script: script}, true
		}
		return nil, false
	}
	t.Cleanup(func() { adapterForAttach = orig })
}

// cloneRemote clones the remote into a directory of its own and moves
// the test there: the working directory whose origin names the project.
func cloneRemote(t *testing.T, remote string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for the attach tests: %v", err)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", remote, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone %s: %v: %s", remote, err, out)
	}
	t.Chdir(clone)
}

// runAttachCmd runs the attach command on the environment's engine and
// returns its buffers and exit-code channel, readable while it runs.
func (e *attachEnv) runAttachCmd(t *testing.T, args ...string) (*syncBuffer, *syncBuffer, chan int) {
	t.Helper()
	out, errOut := &syncBuffer{}, &syncBuffer{}
	args = append([]string{"attach", "--state-dir", e.stateDir, "--no-start"}, args...)
	done := make(chan int, 1)
	go func() { done <- run(context.Background(), args, out, errOut) }()
	return out, errOut, done
}

// releaseMarker writes the marker the scripted session ends on.
func releaseMarker(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("BRIGADA_TEST_MARKERS"), "release"), nil, 0o644); err != nil {
		t.Fatalf("release the session: %v", err)
	}
}

// TestAttachRunsTheSessionAndReleases is the attach's happy path
// (§4.2 item 1): the project is found by the working directory's git
// origin, the session runs in the same scratch directory the runner's
// foreman turns use — nowhere else can it resume the same session —
// with the bridge's config beside it, mode 0600 in a private 0700
// directory, the hold's token reaching the file and the bridge alone
// and nothing the session sees on its command line or in its
// environment. The session ends, and the hold is released with the new
// session id the client generated: the key's row carries it, and the
// config is gone.
func TestAttachRunsTheSessionAndReleases(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	cloneRemote(t, remote)
	markerDir(t)
	e := newAttachEnv(t, remote, "fake", "fake-1")
	offerFakeAdapter(t, writeAttachScript(t))

	out, _, done := e.runAttachCmd(t)
	env := e.scratchFile(t, "env.txt")

	// The session's status bar runs this binary's one-line status on the
	// attach's own state directory (§10), quoted for the shell.
	if want := " status --line --state-dir " + shellQuote(e.stateDir); !strings.Contains(env, "BRIGADA_FAKE_STATUS_LINE='") || !strings.Contains(env, want) {
		t.Errorf("the session's status line command is not this binary's status --line on %s:\n%s", e.stateDir, env)
	}

	// The session ran in the project's foreman scratch directory (§4) —
	// the very directory the runner's turns use, by the shared rule.
	where, err := filepath.EvalSymlinks(e.scratchFile(t, "where.txt"))
	if err != nil {
		t.Fatalf("resolve the session's working directory: %v", err)
	}
	scratch, err := filepath.EvalSymlinks(e.scratch(t))
	if err != nil {
		t.Fatalf("resolve the scratch directory: %v", err)
	}
	if where != scratch {
		t.Errorf("the attached session ran in %q, want the scratch %q", where, scratch)
	}

	// The MCP config: written for the hold's token, 0600 in a private
	// 0700 directory, naming this binary as the bridge over the engine's
	// socket, the token in the bridge's env alone.
	envOf := func(name string) string {
		t.Helper()
		for _, kv := range strings.Split(env, "\n") {
			if v, ok := strings.CutPrefix(kv, name+"="); ok {
				return v
			}
		}
		t.Fatalf("the session's environment has no %s:\n%s", name, env)
		return ""
	}
	config := envOf("BRIGADA_FAKE_MCP_CONFIG")
	if !strings.HasPrefix(config, filepath.Join(e.stateDir, "runner", "mcp")+string(filepath.Separator)) {
		t.Fatalf("the MCP config = %q, want it under the runner's mcp root", config)
	}
	fi, err := os.Stat(config)
	if err != nil {
		t.Fatalf("stat the MCP config while the session runs: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the MCP config's mode = %v, want 0600", fi.Mode().Perm())
	}
	for _, dir := range []string{filepath.Dir(config), filepath.Dir(filepath.Dir(config))} {
		if fi, err = os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v (%v), want 0700", dir, fi.Mode().Perm(), err)
		}
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatalf("read the MCP config: %v", err)
	}
	var cfg struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode the MCP config: %v", err)
	}
	srv, ok := cfg.Servers["brigada"]
	if !ok {
		t.Fatalf("config servers = %v, want one named brigada", cfg.Servers)
	}
	if want := []string{"mcp", "--socket", e.socket}; !slices.Equal(srv.Args, want) {
		t.Errorf("the bridge's args = %v, want %v", srv.Args, want)
	}
	if len(srv.Env) != 1 || srv.Env["BRIGADA_TOKEN"] == "" {
		t.Fatalf("the bridge's env = %v, want the hold token under BRIGADA_TOKEN alone", srv.Env)
	}
	// The token reaches the file and the bridge alone: not the command
	// line — the session's argv is empty — and not the environment the
	// session itself ran with.
	token := srv.Env["BRIGADA_TOKEN"]
	if strings.Contains(env, token) {
		t.Error("the hold token reached the session's environment")
	}
	if argv := e.scratchFile(t, "argv.txt"); argv != "" {
		t.Errorf("the session's argv = %q, want nothing: no token rides a command line", argv)
	}

	// The opening prompt reached the session — the engine's composition,
	// the instructions and the wake template (§4.2 item 1).
	if prompt := e.scratchFile(t, "prompt.txt"); !strings.Contains(prompt, "You are the foreman of one brigada project") ||
		!strings.Contains(prompt, "No events are pending") {
		t.Errorf("the opening prompt is not the engine's composition:\n%s", prompt)
	}

	// The client printed the session's line before the harness took the
	// terminal.
	if line := out.String(); !strings.Contains(line, "attached to the foreman of project "+strconv.FormatInt(e.project, 10)) {
		t.Errorf("stdout = %q, want the attached line naming the project", line)
	}

	// The session ends; the hold is released with the new session id the
	// client generated (§4.2: the client knows it without reading the
	// CLI's files), and the config dies with the attach.
	newID := envOf("BRIGADA_FAKE_SESSION")
	if !uuidRe.MatchString(newID) {
		t.Errorf("the new session id = %q, want a UUID the client generated", newID)
	}
	releaseMarker(t)
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("attach exit = %d, want 0", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the attached session did not end")
	}
	if written := e.queryRow1(t, "SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", fmt.Sprintf("foreman:%d", e.project)); written != newID {
		t.Errorf("the key's session id = %q, want the generated %q written back", written, newID)
	}
	if state := e.latestHoldState(t); state != "released" {
		t.Errorf("the hold's state = %q, want released", state)
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Errorf("the MCP config outlived the attach: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(config)); !os.IsNotExist(err) {
		t.Errorf("the config directory outlived the attach: %v", err)
	}
}

// uuidRe is a version-4 UUID: the shape of the session ids the client
// generates (§4.2 item 1).
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestAttachReleasesWhateverTheExit is the detach rule's other half
// (§4.2 item 1): a harness that dies badly is still a detach — the hold
// is released with the new session id whatever the exit status was,
// and the client answers with the harness's own exit code.
func TestAttachReleasesWhateverTheExit(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	cloneRemote(t, remote)
	markerDir(t)
	e := newAttachEnv(t, remote, "fake", "fake-1")
	script := filepath.Join(t.TempDir(), "attach-fails.sh")
	body := "#!/bin/sh\nenv > env.tmp\nmv env.tmp env.txt\nexit 7\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write the attach script: %v", err)
	}
	offerFakeAdapter(t, script)

	_, _, done := e.runAttachCmd(t)
	select {
	case code := <-done:
		if code != 7 {
			t.Errorf("attach exit = %d, want the harness's 7", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the attached session did not end")
	}
	// The session ended on its generated id all the same: written back,
	// the hold released, the config gone.
	env := e.scratchFile(t, "env.txt")
	var newID string
	for _, kv := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(kv, "BRIGADA_FAKE_SESSION="); ok {
			newID = v
		}
	}
	if !uuidRe.MatchString(newID) {
		t.Fatalf("the new session id = %q, want a UUID the client generated", newID)
	}
	if written := e.queryRow1(t, "SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", fmt.Sprintf("foreman:%d", e.project)); written != newID {
		t.Errorf("the key's session id = %q, want the generated %q written back", written, newID)
	}
	if state := e.latestHoldState(t); state != "released" {
		t.Errorf("the hold's state = %q, want released", state)
	}
}

// TestAttachForwardsTheTerminalsInterrupt is the interrupt rule (§4.2
// item 1): the terminal's SIGINT reaches this process — whose main
// wraps it into the context — and the client's one job is to hand it
// to the harness unchanged: exactly SIGINT, never a SIGTERM of its own
// behind it, and no early end of the beats or the release. The
// scripted harness records the signal it saw, then dies of it — which
// the client answers with 0, a detach — and the hold it held is
// released with the generated session id.
func TestAttachForwardsTheTerminalsInterrupt(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	cloneRemote(t, remote)
	e := newAttachEnv(t, remote, "fake", "fake-1")
	// The script traps INT to record it, then re-raises INT on itself
	// with the trap aside, so it dies of the signal the way a harness
	// the user interrupted does. env.txt is written whole under a temp
	// name and renamed, so the test's polling read never catches it
	// half-written.
	script := filepath.Join(t.TempDir(), "attach-interrupt.sh")
	body := `#!/bin/sh
trap 'echo sigint > signal.txt; trap - INT; kill -INT $$' INT
env > env.tmp
mv env.tmp env.txt
while [ ! -f "$BRIGADA_TEST_MARKERS/release" ]; do sleep 0.05; done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write the attach script: %v", err)
	}
	offerFakeAdapter(t, script)

	_, _, done := e.runAttachCmd(t)
	e.scratchFile(t, "env.txt") // the session is up, its trap armed

	// The terminal's Ctrl-C reaches this process alone; the harness
	// must see it as the SIGINT it is.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	select {
	case code := <-done:
		// A harness that dies of the forwarded signal is a detach, not
		// a failure: the client answers 0, whatever the signal was.
		if code != 0 {
			t.Errorf("attach exit = %d, want 0: a signal-killed harness is a detach", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the attached session did not end after the interrupt")
	}

	// The harness saw SIGINT itself. Had the client sent anything else —
	// a SIGTERM in the interrupt's place — the script has no trap for
	// it and would have died without recording a thing.
	if sig := e.scratchFile(t, "signal.txt"); sig != "sigint" {
		t.Errorf("the harness recorded %q, want sigint: the interrupt must arrive as itself", sig)
	}

	// The beats outlived the interrupt: the hold was still live when
	// the session ended, and the release wrote the generated id back.
	if state := e.latestHoldState(t); state != "released" {
		t.Errorf("the hold's state = %q, want released: an interrupted chat is still a live hold until its detach", state)
	}
	if written := e.queryRow1(t, "SELECT COALESCE(harness_session_id, '') FROM sessions WHERE key = ?", fmt.Sprintf("foreman:%d", e.project)); written == "" || !uuidRe.MatchString(written) {
		t.Errorf("the key's session id = %q, want the generated UUID written back", written)
	}
}

// TestAttachWaitsOutABusyTurn is the busy rule's waiting side (§4.2
// item 1): a live foreman turn makes the attach say so and wait, and
// the turn's end — a cancel, here — lets the next attempt through, the
// session running then as it always would.
func TestAttachWaitsOutABusyTurn(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	cloneRemote(t, remote)
	markerDir(t)
	e := newAttachEnv(t, remote, "fake", "fake-1")
	offerFakeAdapter(t, writeAttachScript(t))
	job := e.seedForemanJob(t)

	origRetry := attachBusyRetry
	attachBusyRetry = 100 * time.Millisecond
	t.Cleanup(func() { attachBusyRetry = origRetry })

	out, _, done := e.runAttachCmd(t)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "busy with a turn (job "+strconv.FormatInt(job, 10)+")") {
		if time.Now().After(deadline) {
			t.Fatalf("stdout = %q, want the busy line naming job %d", out.String(), job)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The turn ends while the client waits; the next attempt attaches.
	if _, err := e.adminClient(t).CancelJob(context.Background(), job); err != nil {
		t.Fatalf("cancel the busy turn: %v", err)
	}
	e.scratchFile(t, "env.txt") // the session started
	releaseMarker(t)
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("attach exit = %d, want 0 after waiting the turn out", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the attached session did not end")
	}
}

// TestAttachForceCancelsTheTurn is --force (§4.2 item 1): the live
// turn is cancelled through the engine's cancel path and the attach
// takes the foreman at once, the session running as it always would.
func TestAttachForceCancelsTheTurn(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	cloneRemote(t, remote)
	markerDir(t)
	e := newAttachEnv(t, remote, "fake", "fake-1")
	offerFakeAdapter(t, writeAttachScript(t))
	job := e.seedForemanJob(t)
	releaseMarker(t) // the session may run straight through

	_, _, done := e.runAttachCmd(t, "--force")
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("attach --force exit = %d, want 0", code)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the forced attach did not end")
	}
	if status := e.queryRow1(t, "SELECT status FROM jobs WHERE id = ?", job); status != "cancelled" {
		t.Errorf("the live turn = %s, want cancelled by the forced attach", status)
	}
	if state := e.latestHoldState(t); state != "released" {
		t.Errorf("the hold's state = %q, want released", state)
	}
}

// TestAttachSecondIsRefused is the one-attach rule (§4.2 item 1): a
// second attach while one holds the foreman is refused clearly, not
// waited out — another terminal owns the session, and only its user
// ends that.
func TestAttachSecondIsRefused(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	cloneRemote(t, remote)
	markerDir(t)
	e := newAttachEnv(t, remote, "fake", "fake-1")
	offerFakeAdapter(t, writeAttachScript(t))

	_, _, first := e.runAttachCmd(t)
	e.scratchFile(t, "env.txt") // the first session is up and holding

	out, errOut, second := e.runAttachCmd(t)
	select {
	case code := <-second:
		if code != 1 {
			t.Errorf("the second attach exit = %d, want 1", code)
		}
		if msg := errOut.String(); !strings.Contains(msg, "already attached elsewhere") {
			t.Errorf("the second attach's stderr = %q, want it to refuse a second live attach", msg)
		}
		if out.String() != "" {
			t.Errorf("the second attach printed to stdout: %q", out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the second attach did not answer")
	}

	releaseMarker(t)
	select {
	case <-first:
	case <-time.After(20 * time.Second):
		t.Fatal("the first session did not end")
	}
}

// TestAttachHarnessWithoutInteractiveFailsClearly is the harness the
// client cannot drive interactively (§4.2 item 1): kimi resumes
// headless but offers no interactive session to attach, and the client
// says so — releasing the hold it took, for it chatted nothing.
func TestAttachHarnessWithoutInteractiveFailsClearly(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	cloneRemote(t, remote)
	e := newAttachEnv(t, remote, "kimi", "kimi-1")

	out, errOut, done := e.runAttachCmd(t)
	select {
	case code := <-done:
		if code != 1 {
			t.Errorf("attach exit = %d, want 1", code)
		}
		msg := errOut.String()
		if !strings.Contains(msg, "kimi") || !strings.Contains(msg, "cannot be attached") {
			t.Errorf("stderr = %q, want a clear refusal naming the harness", msg)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the attach did not answer")
	}
	// The hold it took is gone: a client that cannot chat does not hold
	// the foreman (§4.2 item 1).
	if state := e.latestHoldState(t); state != "released" {
		t.Errorf("the hold's state = %q, want released by the abandoned attach", state)
	}
	if out.String() != "" {
		t.Errorf("the attach printed to stdout: %q", out.String())
	}
}

func TestDefaultAdapterForAttachCodexIsInteractive(t *testing.T) {
	ad, ok := defaultAdapterForAttach("codex")
	if !ok {
		t.Fatal("defaultAdapterForAttach(codex) = unavailable, want available")
	}
	if _, ok := ad.(adapter.Interactive); !ok {
		t.Fatalf("defaultAdapterForAttach(codex) returned %T, want adapter.Interactive", ad)
	}

	for _, harness := range []string{"kimi", "opencode"} {
		ad, ok := defaultAdapterForAttach(harness)
		if harness == "opencode" && ok {
			t.Errorf("defaultAdapterForAttach(%q) = %T, want unavailable", harness, ad)
		}
		if ok {
			if _, interactive := ad.(adapter.Interactive); interactive {
				t.Errorf("defaultAdapterForAttach(%q) returned an interactive adapter", harness)
			}
		}
	}
}

// TestAttachFailsOutsideAProjectsRepo covers the project's absences
// (§4.2 item 1): a working directory that is no repository at all — a
// bare `brigada` included — and a repository whose origin names no
// project the engine knows, then a project with no foreman chosen.
// All fail plainly, naming what to do.
func TestAttachFailsOutsideAProjectsRepo(t *testing.T) {
	markerDir(t)
	remote := testsupport.NewGitRemote(t)

	// Not a repository: a bare brigada and the subcommand both refuse.
	t.Chdir(t.TempDir())
	for _, args := range [][]string{nil, {"attach"}} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr)
		if code != 1 {
			t.Errorf("run(%q) exit = %d, want 1", args, code)
		}
		if msg := stderr.String(); !strings.Contains(msg, "no origin URL") {
			t.Errorf("run(%q) stderr = %q, want it to say the directory has no origin", args, msg)
		}
	}

	// A repository whose origin names no project.
	e := newAttachEnv(t, "https://example.invalid/elsewhere.git", "fake", "fake-1")
	cloneRemote(t, remote)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"attach", "--state-dir", e.stateDir, "--no-start"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("attach exit = %d, want 1", code)
	}
	msg := stderr.String()
	if !strings.Contains(msg, "no project for repo "+remote) {
		t.Errorf("stderr = %q, want it to name the repo with no project", msg)
	}
	if !strings.Contains(msg, "project foreman") {
		t.Errorf("stderr = %q, want it to say what to do next", msg)
	}

	// A project with no foreman chosen.
	e2 := newAttachEnv(t, remote, "", "")
	var stdout2, stderr2 bytes.Buffer
	code = run(context.Background(), []string{"attach", "--state-dir", e2.stateDir, "--no-start"}, &stdout2, &stderr2)
	if code != 1 {
		t.Errorf("attach exit = %d, want 1", code)
	}
	if msg := stderr2.String(); !strings.Contains(msg, "has no foreman") {
		t.Errorf("stderr = %q, want it to say the project has no foreman", msg)
	}
}

// TestNewSessionIDIsAUUID pins the id's shape (§4.2 item 1): random,
// UUID-formed, and different every time — the client's own id for the
// session, spent on the command and the release alike.
func TestNewSessionIDIsAUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		id, err := newSessionID()
		if err != nil {
			t.Fatalf("newSessionID: %v", err)
		}
		if !uuidRe.MatchString(id) {
			t.Errorf("newSessionID() = %q, want a version-4 UUID", id)
		}
		if seen[id] {
			t.Errorf("newSessionID() repeated %q", id)
		}
		seen[id] = true
	}
}

// TestShellQuote: the status bar's command reaches a POSIX shell, so
// every path in it is quoted whole, single quotes included.
func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/brigada": `'/usr/local/bin/brigada'`,
		"/Users/a b/state":       `'/Users/a b/state'`,
		"/tmp/it's here":         `'/tmp/it'\''s here'`,
		"$HOME/`id`":             "'$HOME/`id`'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestStatusLineCommandAbsolutePaths: the status bar runs in the
// foreman's scratch directory, so a relative --state-dir or --config
// the attach was given reaches it absolute, resolved where the attach
// started.
func TestStatusLineCommandAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	got := statusLineCommand(&runtime{stateDir: "state", configPath: "conf/config"})
	for _, want := range []string{
		" --state-dir " + shellQuote(filepath.Join(dir, "state")),
		" --config " + shellQuote(filepath.Join(dir, "conf", "config")),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status line command = %s, want it to carry %s", got, want)
		}
	}
}

func TestAttachClaudeAddsReadingCheckout(t *testing.T) {
	remote := testsupport.NewGitRemote(t)
	cloneRemote(t, remote)
	e := newAttachEnv(t, remote, "claude", "sonnet")
	script := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\0' \"$@\" > argv.txt\npwd > where.txt\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := adapterForAttach
	adapterForAttach = func(harness string) (adapter.Adapter, bool) {
		return &claude.Adapter{Binary: script}, harness == "claude"
	}
	t.Cleanup(func() { adapterForAttach = orig })
	_, errOut, done := e.runAttachCmd(t)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("attach exit %d: %s", code, errOut.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("attach did not end")
	}
	scratch := e.scratch(t)
	repo := filepath.Join(scratch, "repo")
	argv := strings.Split(e.scratchFile(t, "argv.txt"), "\x00")
	i := slices.Index(argv, "--add-dir")
	if i < 0 || i+1 >= len(argv) || argv[i+1] != repo {
		t.Fatalf("attach lacks --add-dir %s: %v", repo, argv)
	}
	if !strings.Contains(argv[0], repo) || !strings.Contains(argv[0], `Branch: "main"`) {
		t.Errorf("opening prompt lacks checkout: %s", argv[0])
	}
	if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
		t.Fatal(err)
	}
	where, err := filepath.EvalSymlinks(e.scratchFile(t, "where.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(scratch)
	if err != nil || where != want {
		t.Errorf("cwd = %s, want %s: %v", where, want, err)
	}
}
