package mcp

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
)

func startInteractive(t *testing.T, e *env, b *Interactive) (*session, <-chan error) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- (&Server{Interactive: b}).Serve(context.Background(), inR, outW); outW.Close() }()
	t.Cleanup(func() { inW.Close(); outR.Close() })
	return &session{t: t, w: inW, r: bufio.NewReader(outR)}, done
}
func interactiveCall(s *session, id int, name string, args any, meta any) answer {
	return s.call(id, "tools/call", map[string]any{"name": name, "arguments": args, "_meta": meta})
}
func codexMeta(session string, urls ...string) any {
	return map[string]any{"x-codex-turn-metadata": map[string]any{"session_id": session, "git_remote_urls": urls}}
}
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	end := time.Now().Add(4 * time.Second)
	for time.Now().Before(end) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestInteractiveCodexSelectionMetadataAndEOF(t *testing.T) {
	e := newEnv(t)
	// A second project must never appear in the selected project's read tools.
	if err := e.st.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (repo_url,default_branch,settings,foreman_harness,foreman_model,created_at,updated_at) VALUES ('https://other.invalid/y.git','main','{}','claude','sonnet',?,?)`, seedTS, seedTS)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	b := NewInteractive(e.client, "", t.TempDir(), "")
	s, done := startInteractive(t, e, b)
	meta := codexMeta("codex-thread", "git@EXAMPLE.invalid:x.git")
	text, bad := toolText(t, interactiveCall(s, 1, "status", map[string]any{}, meta))
	if bad {
		t.Fatal(text)
	}
	if n := countOf(t, e, "SELECT COUNT(*) FROM interactive_holds"); n != 0 {
		t.Fatal("read acquired a hold")
	}
	text, bad = toolText(t, interactiveCall(s, 2, "decision_note", map[string]any{"text": "Choose a small API."}, meta))
	if bad {
		t.Fatal(text)
	}
	if n := countOf(t, e, "SELECT COUNT(*) FROM interactive_holds WHERE state='live' AND project_id=1"); n != 1 {
		t.Fatal("implicit acquire did not select project 1")
	}
	for query, want := range map[string]string{
		"SELECT harness_session_id FROM interactive_holds":     "codex-thread",
		"SELECT harness_session_source FROM interactive_holds": "request_meta",
		"SELECT bridge_instance FROM interactive_holds":        b.BridgeInstance,
	} {
		if got := strOf(t, e, query); got != want {
			t.Fatalf("%s=%q want %q", query, got, want)
		}
	}
	if strOf(t, e, "SELECT harness_session_observed_at FROM interactive_holds") == "" {
		t.Fatal("no observed time")
	}
	// The verified session never becomes a session_hint or mutation credential.
	if got := strOf(t, e, "SELECT COALESCE(session_hint,'') FROM interactive_holds"); got != "" {
		t.Fatal(got)
	}
	text, bad = toolText(t, interactiveCall(s, 3, "hold_acquire", map[string]any{}, meta))
	if bad || !strings.Contains(text, "decision_notes") || strings.Contains(text, "credential") {
		t.Fatalf("digest=%q bad=%v", text, bad)
	}
	s.w.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EOF did not stop")
	}
	if n := countOf(t, e, "SELECT COUNT(*) FROM interactive_holds WHERE state='live'"); n != 0 {
		t.Fatal("EOF did not yield")
	}
}

func TestInteractiveClaudeSubdirectory(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "ssh://git@EXAMPLE.invalid:22/x.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	sub := filepath.Join(dir, "src", "nested")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", sub)
	b := NewInteractive(e.client, "", sub, "claude-session")
	s, _ := startInteractive(t, e, b)
	text, bad := toolText(t, interactiveCall(s, 1, "hold_acquire", map[string]any{}, nil))
	if bad {
		t.Fatal(text)
	}
	if strOf(t, e, "SELECT harness_session_id FROM interactive_holds") != "claude-session" || strOf(t, e, "SELECT harness_session_source FROM interactive_holds") != "env" {
		t.Fatal("wrong Claude metadata")
	}
}

func TestInteractiveSelectionFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		repo      string
		meta      any
		duplicate bool
	}{
		{"missing", "", codexMeta("s", "https://unknown.invalid/z"), false},
		{"no remotes", "", codexMeta("s"), false},
		{"two matches", "", codexMeta("s", "https://example.invalid/x", "https://other.invalid/y"), false},
		{"ambiguous alias", "", codexMeta("s", "git@alias:x"), false},
		{"duplicate canonical projects", "https://example.invalid/x", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := newEnv(t)
			other := "https://other.invalid/y"
			if test.duplicate {
				other = "git@example.invalid:x.git"
			}
			if err := e.st.Update(context.Background(), func(tx *sql.Tx) error {
				_, err := tx.Exec(`INSERT INTO projects (repo_url,default_branch,settings,foreman_harness,foreman_model,created_at,updated_at) VALUES (?,'main','{}','claude','sonnet',?,?)`, other, seedTS, seedTS)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			s, _ := startInteractive(t, e, NewInteractive(e.client, test.repo, t.TempDir(), ""))
			text, bad := toolText(t, interactiveCall(s, 1, "decision_note", map[string]any{"text": "must not apply"}, test.meta))
			if !bad || !strings.Contains(text, "brigada init") {
				t.Fatalf("%q bad=%v", text, bad)
			}
			// A later good URL cannot replace selection from the first request.
			_, bad = toolText(t, interactiveCall(s, 2, "hold_acquire", nil, codexMeta("s", "https://example.invalid/x")))
			if !bad || countOf(t, e, "SELECT COUNT(*) FROM interactive_holds") != 0 {
				t.Fatal("selection guessed or changed")
			}
		})
	}
}

func TestInteractiveExplicitOverrideAndChangedCaller(t *testing.T) {
	e := newEnv(t)
	s, _ := startInteractive(t, e, NewInteractive(e.client, "example.invalid/x", t.TempDir(), ""))
	text, bad := toolText(t, interactiveCall(s, 1, "hold_acquire", nil, codexMeta("s", "https://unknown.invalid/ignored")))
	if bad {
		t.Fatal(text)
	}
	text, bad = toolText(t, interactiveCall(s, 2, "decision_note", map[string]any{"text": "do not write"}, codexMeta("another", "https://example.invalid/x")))
	if !bad || !strings.Contains(text, "hold_lost") || countOf(t, e, "SELECT COUNT(*) FROM messages WHERE kind='decision_note'") != 0 {
		t.Fatalf("changed caller: %q bad=%v", text, bad)
	}
	text, bad = toolText(t, interactiveCall(s, 3, "hold_acquire", nil, codexMeta("s")))
	if !bad || !strings.Contains(text, "hold_lost") {
		t.Fatal("shared bridge reacquired")
	}
}

func TestInteractiveRenewalRefusalRequiresExplicitAcquire(t *testing.T) {
	e := newEnv(t)
	b := NewInteractive(e.client, "https://example.invalid/x", t.TempDir(), "")
	s, _ := startInteractive(t, e, b)
	if text, bad := toolText(t, interactiveCall(s, 1, "hold_acquire", nil, nil)); bad {
		t.Fatal(text)
	}
	// Force a short renewal deadline without waiting for the production lease.
	b.mu.Lock()
	b.hold.LeaseExpires = time.Now().Add(600 * time.Millisecond)
	b.mu.Unlock()
	if _, err := e.client.ReleaseHold(context.Background(), "https://example.invalid/x.git"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.lost })
	for i, name := range []string{"decision_note", "status", "hold_yield"} {
		text, bad := toolText(t, interactiveCall(s, 2+i, name, map[string]any{"text": "no replay"}, nil))
		if !bad || !strings.Contains(text, "hold_lost") {
			t.Fatalf("%s: %q bad=%v", name, text, bad)
		}
	}
	if countOf(t, e, "SELECT COUNT(*) FROM interactive_holds") != 1 {
		t.Fatal("heartbeat or tool reacquired")
	}
	if text, bad := toolText(t, interactiveCall(s, 6, "hold_acquire", nil, nil)); bad {
		t.Fatal(text)
	}
	if countOf(t, e, "SELECT COUNT(*) FROM messages WHERE kind='decision_note'") != 0 {
		t.Fatal("replayed mutation")
	}
	if text, bad := toolText(t, interactiveCall(s, 7, "decision_note", map[string]any{"text": "fresh decision"}, nil)); bad {
		t.Fatal(text)
	}
}

func TestInteractiveStaleMutationNeverRetries(t *testing.T) {
	e := newEnv(t)
	s, _ := startInteractive(t, e, NewInteractive(e.client, "https://example.invalid/x", t.TempDir(), ""))
	if text, bad := toolText(t, interactiveCall(s, 1, "hold_acquire", nil, nil)); bad {
		t.Fatal(text)
	}
	if _, err := e.client.ReleaseHold(context.Background(), "https://example.invalid/x.git"); err != nil {
		t.Fatal(err)
	}
	text, bad := toolText(t, interactiveCall(s, 2, "decision_note", map[string]any{"text": "stale"}, nil))
	if !bad || !strings.Contains(text, "stale_hold") {
		t.Fatalf("%q bad=%v", text, bad)
	}
	text, bad = toolText(t, interactiveCall(s, 3, "decision_note", map[string]any{"text": "queued"}, nil))
	if !bad || !strings.Contains(text, "hold_lost") || countOf(t, e, "SELECT COUNT(*) FROM interactive_holds") != 1 {
		t.Fatal("retried stale hold")
	}
}

// The test process acts as the harness for a real child bridge over stdio.
func TestInteractiveChildBridge(t *testing.T) {
	if os.Getenv("BRIGADA_MCP_TEST_CHILD") != "1" {
		return
	}
	b := NewInteractive(api.NewClient(os.Getenv("BRIGADA_TEST_SOCKET"), "client-secret-9"), "https://example.invalid/x", "", os.Getenv("CLAUDE_CODE_SESSION_ID"))
	err := (&Server{Interactive: b}).Serve(context.Background(), os.Stdin, os.Stdout)
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
func TestInteractiveProcessEOF(t *testing.T) {
	e := newEnv(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestInteractiveChildBridge$")
	cmd.Env = append(os.Environ(), "BRIGADA_MCP_TEST_CHILD=1", "BRIGADA_TEST_SOCKET="+e.socket, "CLAUDE_CODE_SESSION_ID=process-session")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	s := &session{t: t, w: in, r: bufio.NewReader(out)}
	text, bad := toolText(t, interactiveCall(s, 1, "hold_acquire", nil, nil))
	if bad {
		t.Fatal(text)
	}
	in.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if countOf(t, e, "SELECT COUNT(*) FROM interactive_holds WHERE state='live'") != 0 {
		t.Fatal("child EOF did not release")
	}
}

func TestCodexEncodedMetadata(t *testing.T) {
	m, _ := json.Marshal(map[string]any{"session_id": "s", "workspace_roots": []any{map[string]any{"git_remote_urls": []string{"https://example.invalid/x"}}}})
	meta, _ := json.Marshal(map[string]any{"x-codex-turn-metadata": string(m)})
	session, urls, ok := codexMetadata(meta)
	if !ok || session != "s" || len(urls) != 1 {
		t.Fatalf("session=%q urls=%v ok=%v", session, urls, ok)
	}
}

func TestInteractiveToolSet(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range InteractiveTools() {
		names[tool.Name] = true
	}
	for _, name := range []string{"plan_create", "plan_amend", "task_restart", "task_cancel", "answer", "ask_user", "events", "status", "stats", "history", "capacity", "hold_acquire", "hold_yield", "decision_note"} {
		if !names[name] {
			t.Errorf("missing tool %s", name)
		}
	}
	for _, name := range []string{"plan_approve", "plan_reject", "ask_foreman"} {
		if names[name] {
			t.Errorf("excess authority: %s", name)
		}
	}
}

func TestInteractiveDetectsUnattributedOverlap(t *testing.T) {
	b := NewInteractive(nil, "", "", "")
	line := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"events"}}`)
	b.incoming(line)
	b.incoming(line)
	if !b.shared.Load() {
		t.Fatal("unattributed overlap not detected")
	}
	b.answered(line)
	b.answered(line)
}

func TestHookFenceSnapshotCannotYieldReplacement(t *testing.T) {
	e := newEnv(t)
	state := filepath.Dir(e.socket)
	b := NewInteractive(e.client, "https://example.invalid/x", t.TempDir(), "same-session")
	if err := b.StartHook(state, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	s, _ := startInteractive(t, e, b)
	if text, bad := toolText(t, interactiveCall(s, 1, "hold_acquire", nil, nil)); bad {
		t.Fatal(text)
	}
	raw, err := os.ReadFile(b.hook.record)
	if err != nil {
		t.Fatal(err)
	}
	var old HookBinding
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "credential") || strings.Contains(string(raw), "client-secret") {
		t.Fatal("secret persisted in hook binding")
	}
	// The same process explicitly reacquires, keeping session metadata but
	// advancing the fence. An already-started hook still quotes its old fence.
	if text, bad := toolText(t, interactiveCall(s, 2, "hold_acquire", nil, nil)); bad {
		t.Fatal(text)
	}
	if err := yieldBinding(context.Background(), old); err == nil || !strings.Contains(err.Error(), "stale_hold") {
		t.Fatalf("old fence accepted: %v", err)
	}
	if countOf(t, e, "SELECT COUNT(*) FROM interactive_holds WHERE state='live'") != 1 {
		t.Fatal("stale hook yielded replacement")
	}
	if err := YieldFromHook(context.Background(), state, "same-session"); err != nil {
		t.Fatal(err)
	}
	if countOf(t, e, "SELECT COUNT(*) FROM interactive_holds WHERE state='live'") != 0 {
		t.Fatal("current binding did not yield")
	}
	// A new bridge with the same session id is a different holder. The old
	// bridge socket and hold reference cannot authorize a release of it.
	other := NewInteractive(e.client, "https://example.invalid/x", t.TempDir(), "same-session")
	s2, _ := startInteractive(t, e, other)
	if text, bad := toolText(t, interactiveCall(s2, 3, "hold_acquire", nil, nil)); bad {
		t.Fatal(text)
	}
	if err := yieldBinding(context.Background(), old); err == nil {
		t.Fatal("old bridge accepted stale hook")
	}
	if countOf(t, e, "SELECT COUNT(*) FROM interactive_holds WHERE state='live'") != 1 {
		t.Fatal("old bridge yielded new bridge")
	}
}

func TestHookSessionAloneDoesNotSelectBridge(t *testing.T) {
	e := newEnv(t)
	state := filepath.Dir(e.socket)
	b := NewInteractive(e.client, "https://example.invalid/x", t.TempDir(), "same-session")
	// This is deliberately not an ancestor harness of the hook process.
	if err := b.StartHook(state, 2147483647); err != nil {
		t.Fatal(err)
	}
	s, _ := startInteractive(t, e, b)
	if text, bad := toolText(t, interactiveCall(s, 1, "hold_acquire", nil, nil)); bad {
		t.Fatal(text)
	}
	if err := YieldFromHook(context.Background(), state, "same-session"); err == nil {
		t.Fatal("session id alone selected another process's bridge")
	}
	if countOf(t, e, "SELECT COUNT(*) FROM interactive_holds WHERE state='live'") != 1 {
		t.Fatal("unrelated hook released hold")
	}
}

func TestInteractiveScopedReadsAndAdditionalTools(t *testing.T) {
	e := newEnv(t)
	if err := e.st.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (repo_url,default_branch,settings,foreman_harness,foreman_model,created_at,updated_at) VALUES ('https://other.invalid/y','main','{}','claude','sonnet',?,?)`, seedTS, seedTS)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO plans (project_id,title,state,created_at,updated_at) VALUES (2,'private-other-plan','active',?,?)`, seedTS, seedTS)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s, _ := startInteractive(t, e, NewInteractive(e.client, "https://example.invalid/x", t.TempDir(), ""))
	for i, name := range []string{"status", "history", "capacity", "stats"} {
		text, bad := toolText(t, interactiveCall(s, i+1, name, map[string]any{}, nil))
		if bad || strings.Contains(text, "private-other-plan") {
			t.Fatalf("%s: %q bad=%v", name, text, bad)
		}
	}
	if countOf(t, e, "SELECT COUNT(*) FROM interactive_holds") != 0 {
		t.Fatal("reads acquired")
	}
	text, bad := toolText(t, interactiveCall(s, 5, "ask_user", map[string]any{"text": "Choose a default?"}, nil))
	if bad || !strings.Contains(text, "question") {
		t.Fatalf("%q bad=%v", text, bad)
	}
	if countOf(t, e, "SELECT COUNT(*) FROM questions WHERE project_id=1 AND answer IS NULL") != 1 {
		t.Fatal("question not journaled")
	}
	text, bad = toolText(t, interactiveCall(s, 6, "ask_user", map[string]any{"text": "Choose a default?"}, nil))
	if bad || countOf(t, e, "SELECT COUNT(*) FROM questions") != 1 {
		t.Fatal("ask_user not idempotent")
	}
}

func TestInteractiveEOFCancellationWithoutEngine(t *testing.T) {
	b := NewInteractive(nil, "", "", "")
	var out strings.Builder
	if err := (&Server{Interactive: b}).Serve(context.Background(), strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveContextStopsWithOpenStdin(t *testing.T) {
	b := NewInteractive(nil, "", "", "")
	ctx, cancel := context.WithCancel(context.Background())
	in, out := io.Pipe()
	defer in.Close()
	defer out.Close()
	done := make(chan error, 1)
	go func() { done <- (&Server{Interactive: b}).Serve(ctx, in, io.Discard) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("signal context did not stop an idle bridge")
	}
}

func TestQueuedMutationMustBeDecidedAgainAfterAcquire(t *testing.T) {
	b := NewInteractive(nil, "", "", "")
	b.first, b.project = true, "example.invalid/x"
	b.acquiredAt = time.Now()
	ctx := context.WithValue(context.Background(), receivedKey{}, b.acquiredAt.Add(-time.Second))
	called := false
	tool := Tool{Name: "decision_note", Call: func(context.Context, *api.Client, json.RawMessage) (string, error) {
		called = true
		return "applied", nil
	}}
	text, err := b.call(ctx, tool, json.RawMessage(`{"text":"queued"}`), nil)
	if err == nil || !strings.Contains(err.Error(), "queued before acquire") || called || text != "" {
		t.Fatalf("queued mutation replayed: %q err=%v called=%v", text, err, called)
	}
}

func TestInteractiveBusyErrorNamesHolder(t *testing.T) {
	err := &api.StatusError{APICode: "foreman_busy", Status: 409, Message: "foreman turn since noon", Body: []byte(`{"holder":{"state":"headless turn","holder_id":42,"label":"foreman turn","since":"noon","events":["worker question"]}}`)}
	text := errorText(err)
	for _, want := range []string{"foreman_busy", "42", "noon", "worker question"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
}

func TestCodexWorkspacePathsAreNotRemoteURLs(t *testing.T) {
	raw := json.RawMessage(`{"x-codex-turn-metadata":{"session_id":"s","workspace_roots":["/local/repo"],"git_remote_urls":["https://example.invalid/x.git"],"model":"not-a-url"}}`)
	_, urls, ok := codexMetadata(raw)
	if !ok || len(urls) != 1 || urls[0] != "https://example.invalid/x.git" {
		t.Fatalf("remote selectors=%v", urls)
	}
}

func TestUnknownFirstToolStillFreezesMetadata(t *testing.T) {
	b := NewInteractive(nil, "", "", "")
	srv := &Server{Interactive: b}
	_, err := srv.callTool(context.Background(), json.RawMessage(`{"name":"unknown","_meta":{"x-codex-turn-metadata":{"session_id":"first","git_remote_urls":["https://example.invalid/first"]}}}`))
	if err == nil {
		t.Fatal("unknown tool accepted")
	}
	b.mu.Lock()
	b.observeFirst(context.Background(), json.RawMessage(`{"x-codex-turn-metadata":{"session_id":"later","git_remote_urls":["https://example.invalid/later"]}}`))
	b.mu.Unlock()
	if b.SessionID != "first" || len(b.candidates) != 1 || b.candidates[0] != "https://example.invalid/first" {
		t.Fatal("first tools/call metadata was replaced")
	}
}
