//go:build qwenlive

package qwencode_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/qwencode"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/mcp"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The foreman checks run the real, signed-in qwen CLI: each is one
// headless model call, so the pair spends two of the five live runs the
// issue allows. Run them by hand, never in the default check:
//
//	go test -tags qwenlive -count=1 -run 'TestLiveForemanBridge|TestLiveForkSession' -v ./internal/adapter/qwencode
//
// The engine behind the bridge is stood up inside the test, on a temp
// socket and in a temp state directory; nothing reaches the user's engine
// or any real project token.

// liveForemanToken is the bridge's token in the test engine's token file:
// a foreman lease scoped to the one project the test seeds.
const (
	liveForemanToken = "f1 foreman:1 foreman-secret-9\n"
	liveForemanKey   = "foreman-secret-9"
	liveSeedTS       = "2026-10-13T12:00:00.000000000Z"
)

// Run 1 hands its session, its working directory and its nonce to the fork
// check, which runs after it in the same invocation.
var (
	liveRepo    string
	liveSession string
	liveNonce   string
)

// standUpBridge starts a test engine and its API on a temp socket, builds
// the brigada binary the bridge runs, and writes the harness config that
// points a turn at the bridge. It returns the config's path and body.
func standUpBridge(t *testing.T) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	state := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(state, "store.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tokenPath := filepath.Join(state, "tokens")
	if err := os.WriteFile(tokenPath, []byte(liveForemanToken), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	planReview := false
	srv, err := api.New(ctx, st, engine.New(st, engine.Config{PlanReview: &planReview}), nil, tokenPath)
	if err != nil {
		t.Fatalf("build api server: %v", err)
	}
	err = st.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/x.git', 'main', '{}', 'qwen-code', 'default', ?, ?)`, liveSeedTS, liveSeedTS)
		return err
	})
	if err != nil {
		t.Fatalf("seed the engine: %v", err)
	}
	// The socket lives outside t.TempDir(): its name carries the test's, and
	// a long one pushes the path past the kernel's sun_path cap.
	socket := testsupport.ShortSocketPath(t)
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Listener = l
	hs.Start()
	t.Cleanup(hs.Close)

	bin := filepath.Join(state, "brigada")
	build := exec.Command("go", "build", "-o", bin, "./cmd/brigada")
	build.Dir = "../../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the brigada binary: %v\n%s", err, out)
	}
	data, err := mcp.HarnessConfig(bin, socket, liveForemanKey)
	if err != nil {
		t.Fatalf("render the harness config: %v", err)
	}
	path := filepath.Join(state, "mcp.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write the harness config: %v", err)
	}
	return path, data
}

// preflightBridge runs the configured bridge command itself, over stdio,
// before any qwen call: initialize, then one status call. A broken bridge
// fails here, without spending a live run.
func preflightBridge(t *testing.T, data []byte) {
	t.Helper()
	var cfg struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode the harness config: %v", err)
	}
	srv, ok := cfg.Servers[mcp.ServerName]
	if !ok {
		t.Fatalf("harness config names no %s server", mcp.ServerName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)
	for k, v := range srv.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the bridge: %v", err)
	}
	defer func() {
		stdin.Close()
		cmd.Wait()
	}()
	r := bufio.NewReader(stdout)
	for _, step := range []struct{ line, want string }{
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`, `"name":"brigada"`},
		{`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"status","arguments":{}}}`, "engine pid"},
	} {
		if _, err := fmt.Fprintln(stdin, step.line); err != nil {
			t.Fatalf("write to the bridge: %v", err)
		}
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read the bridge's answer: %v", err)
		}
		if !strings.Contains(line, step.want) {
			t.Fatalf("bridge answer %q lacks %q", line, step.want)
		}
	}
}

// newUUID is a random version-4 UUID: the id the client mints for a fork.
func newUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// runQwen runs a command the adapter built and returns its stdout lines and
// the events the adapter parsed from them. A non-zero exit fails the test.
func runQwen(t *testing.T, built *exec.Cmd) ([]string, []adapter.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, built.Path, built.Args[1:]...)
	cmd.Dir, cmd.Env = built.Dir, built.Env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a := &qwencode.Adapter{}
	var (
		mu     sync.Mutex
		lines  []string
		events []adapter.Event
		wg     sync.WaitGroup
	)
	read := func(r io.Reader, parse func([]byte) (adapter.Event, bool), keep bool) {
		defer wg.Done()
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for s.Scan() {
			line := append([]byte(nil), s.Bytes()...)
			mu.Lock()
			if keep {
				lines = append(lines, string(line))
			}
			if ev, ok := parse(line); ok {
				events = append(events, ev)
			}
			mu.Unlock()
		}
	}
	wg.Add(2)
	go read(stdout, a.ParseEvent, true)
	go read(stderr, a.ParseErrEvent, false)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("qwen exited: %v", err)
	}
	return lines, events
}

// initLine returns the stream's init line, the first system/init object.
func initLine(lines []string) ([]byte, bool) {
	for _, l := range lines {
		var head struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal([]byte(l), &head) == nil && head.Type == "system" && head.Subtype == "init" {
			return []byte(l), true
		}
	}
	return nil, false
}

// brigadaConnected reports whether the init line lists the brigada server
// with a status that says connected.
func brigadaConnected(init []byte) bool {
	var v struct {
		McpServers []map[string]any `json:"mcp_servers"`
	}
	if json.Unmarshal(init, &v) != nil {
		return false
	}
	for _, s := range v.McpServers {
		if s["name"] != mcp.ServerName {
			continue
		}
		for _, val := range s {
			if val == "connected" {
				return true
			}
		}
	}
	return false
}

// TestLiveForemanBridge: one headless foreman turn through the adapter's
// Command with the brigada bridge attached. The init event lists the
// bridge connected, and the stream shows the model calling its status
// tool. The session it leaves is the fork check's starting point.
func TestLiveForemanBridge(t *testing.T) {
	cfgPath, cfgData := standUpBridge(t)
	preflightBridge(t, cfgData)

	repo, err := os.MkdirTemp("", "bq-repo-")
	if err != nil {
		t.Fatal(err)
	}
	// Not removed here: the fork check resumes in this directory and
	// removes it when it ends.
	liveRepo = repo
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.invalid"}, {"config", "user.name", "t"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	nonce := newUUID(t)[:8]
	job := adapter.Job{
		Prompt:    "Remember this word for later: " + nonce + ". Then call the brigada status tool once, and reply with only the word done.",
		MCPConfig: cfgPath,
	}
	built, err := (&qwencode.Adapter{}).Command(job, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	lines, events := runQwen(t, built)

	init, ok := initLine(lines)
	if !ok {
		t.Fatalf("no system/init line in the stream (%d lines)", len(lines))
	}
	if !brigadaConnected(init) {
		t.Errorf("init line does not list the brigada server connected: %s", init)
	}
	var calledStatus bool
	var session string
	for _, ev := range events {
		if ev.Kind == adapter.EvToolStart && ev.Text == "mcp__brigada__status" {
			calledStatus = true
		}
		if ev.Kind == adapter.EvError {
			t.Errorf("error event: %+v", ev)
		}
		if ev.SessionID != "" {
			session = ev.SessionID
		}
	}
	if !calledStatus {
		t.Error("the stream shows no mcp__brigada__status tool call")
	}
	if session == "" {
		t.Fatal("no session id")
	}
	if strings.TrimSpace((&qwencode.Adapter{}).ResultText(events)) == "" {
		t.Error("empty result text")
	}
	liveSession, liveNonce = session, nonce
}

// TestLiveForkSession: the one open behaviour of the attach, settled
// headlessly. Run 1's session is resumed with --fork-session onto a new
// client-minted id. The init event must name that id, its transcript must
// exist under ~/.qwen/projects, and the fork must remember run 1's word.
func TestLiveForkSession(t *testing.T) {
	if liveSession == "" {
		t.Skip("run 1 left no session: see TestLiveForemanBridge")
	}
	repo := liveRepo
	t.Cleanup(func() { os.RemoveAll(repo) })
	fresh := newUUID(t)

	job := adapter.Job{Prompt: "What was the word I asked you to remember earlier? Reply with only that word."}
	built, err := (&qwencode.Adapter{}).Command(job, repo, liveSession)
	if err != nil {
		t.Fatal(err)
	}
	built.Args = append(built.Args, "--fork-session", "--session-id", fresh)
	lines, events := runQwen(t, built)

	init, ok := initLine(lines)
	if !ok {
		t.Fatalf("no system/init line in the fork's stream")
	}
	var v struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(init, &v); err != nil {
		t.Fatalf("decode the init line: %v", err)
	}
	if v.SessionID != fresh {
		t.Errorf("init session_id = %q, want the minted %q (the fork did not land on it)", v.SessionID, fresh)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(home, ".qwen", "projects", "*", fresh+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Errorf("no transcript %s.jsonl under ~/.qwen/projects", fresh)
	}

	if got := (&qwencode.Adapter{}).ResultText(events); !strings.Contains(got, liveNonce) {
		t.Errorf("the fork did not recall run 1's word %q; its answer was %q", liveNonce, got)
	}
}
