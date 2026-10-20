package mcp

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The bridge's tests run it over an in-process pipe — io.Pipe on both
// sides, the protocol's own shape — against a real engine and its API
// on a temp Unix socket, the way a foreman's harness drives it.

// seedTS and the token file body mirror the other packages' test envs.
const seedTS = "2026-10-13T12:00:00.000000000Z"

const tokenFileBody = "c1 client client-secret-9\n"

// foremanTokenFile scopes the bridge's token to the seeded project —
// the role a real bridge runs under (§12.2), and the only one that may
// create plans without naming a repo.
const foremanTokenFile = "f1 foreman:1 foreman-secret-9\n"

// env is one test engine: store, engine and API on a Unix socket, with
// a project, one completed job and its timeline seeded for the tools
// to read.
type env struct {
	t      *testing.T
	socket string
	client *api.Client
	st     *store.Store
}

func newEnv(t *testing.T) *env {
	return newEnvToken(t, tokenFileBody, "client-secret-9")
}

// newEnvToken is newEnv with the bridge's client bearing one given
// secret of the token file the engine loads.
func newEnvToken(t *testing.T, tokens, secret string) *env {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(tmp, "store.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tokenPath := filepath.Join(tmp, "tokens")
	if err := os.WriteFile(tokenPath, []byte(tokens), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	srv, err := api.New(ctx, st, engine.New(st, engine.Config{}), nil, tokenPath)
	if err != nil {
		t.Fatalf("build api server: %v", err)
	}
	// Not under t.TempDir(): its name carries the test's, and a long one
	// pushes a socket path past the kernel's sun_path cap.
	socket := testsupport.ShortSocketPath(t)
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hs := httptest.NewUnstartedServer(srv.Handler())
	hs.Listener = l
	hs.Start()
	t.Cleanup(hs.Close)

	e := &env{t: t, socket: socket, client: api.NewClient(socket, secret), st: st}
	err = st.Update(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
			VALUES ('https://example.invalid/x.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, seedTS, seedTS)
		if err != nil {
			return err
		}
		project, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// One completed job, its event and the message it produced:
		// enough timeline for the history tool to render a line a test
		// can name, and a done job for status to count.
		res, err = tx.Exec(`INSERT INTO jobs (project_id, kind, key, mode, prompt, status, created_at, updated_at)
			VALUES (?, 'foreman', 'foreman:1', 'session', 'wake', 'done', ?, ?)`, project, seedTS, seedTS)
		if err != nil {
			return err
		}
		job, err := res.LastInsertId()
		if err != nil {
			return err
		}
		res, err = tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('job_completed', ?, NULL, ?, '{"answer":"ok"}', ?)`, project, job, seedTS)
		if err != nil {
			return err
		}
		event, err := res.LastInsertId()
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO messages (project_id, event_id, kind, author, content, created_at)
			VALUES (?, ?, 'worker_result', 'worker', '{"status":"done","summary":"answered"}', ?)`,
			project, event, seedTS)
		return err
	})
	if err != nil {
		t.Fatalf("seed the engine: %v", err)
	}
	return e
}

// session is one bridge over in-process pipes: lines go in, answers
// come out, each under the id the test watches for.
type session struct {
	t *testing.T
	w io.WriteCloser
	r *bufio.Reader
}

// start runs the bridge over pipes. Closing the input ends the server
// when the test does.
func start(t *testing.T, e *env) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := &Server{Client: e.client}
	go func() { _ = srv.Serve(context.Background(), inR, outW) }()
	t.Cleanup(func() {
		inW.Close()
		outW.Close()
	})
	return &session{t: t, w: inW, r: bufio.NewReader(outR)}
}

// send writes one request line.
func (s *session) send(id int, method string, params any) {
	s.t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		body["params"] = params
	}
	s.sendLine(body)
}

// notify writes one notification line — a request without the id.
func (s *session) notify(method string) {
	s.t.Helper()
	s.sendLine(map[string]any{"jsonrpc": "2.0", "method": method})
}

// sendLine writes one already-shaped protocol message.
func (s *session) sendLine(body any) {
	s.t.Helper()
	line, err := json.Marshal(body)
	if err != nil {
		s.t.Fatalf("marshal the line: %v", err)
	}
	if _, err := s.w.Write(append(line, '\n')); err != nil {
		s.t.Fatalf("write the line: %v", err)
	}
}

// answer is one decoded response line.
type answer struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// recv reads one response line.
func (s *session) recv() answer {
	s.t.Helper()
	line, err := s.r.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("read an answer: %v", err)
	}
	var a answer
	if err := json.Unmarshal(line, &a); err != nil {
		s.t.Fatalf("decode the answer %s: %v", line, err)
	}
	return a
}

// call sends one request and returns its answer, checked for its id.
func (s *session) call(id int, method string, params any) answer {
	s.t.Helper()
	s.send(id, method, params)
	a := s.recv()
	var got int
	if err := json.Unmarshal(a.ID, &got); err != nil || got != id {
		s.t.Fatalf("answer id = %s, want %d", a.ID, id)
	}
	return a
}

// toolText reads a tools/call answer's single text block.
func toolText(t *testing.T, a answer) (text string, isError bool) {
	t.Helper()
	if a.Error != nil {
		t.Fatalf("tools/call answered a protocol error: %d %s", a.Error.Code, a.Error.Message)
	}
	var res toolResult
	if err := json.Unmarshal(a.Result, &res); err != nil {
		t.Fatalf("decode the tool result %s: %v", a.Result, err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("tool result content = %+v, want one text block", res.Content)
	}
	return res.Content[0].Text, res.IsError
}

// TestServeInitialize covers the handshake (§12.2): the server names
// itself brigada, speaks a protocol version, and offers the tools
// capability — nothing more.
func TestServeInitialize(t *testing.T) {
	s := start(t, newEnv(t))
	a := s.call(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
	})
	if a.Error != nil {
		t.Fatalf("initialize: %d %s", a.Error.Code, a.Error.Message)
	}
	var res struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(a.Result, &res); err != nil {
		t.Fatalf("decode initialize result %s: %v", a.Result, err)
	}
	if res.ServerInfo.Name != "brigada" {
		t.Errorf("server name = %q, want brigada", res.ServerInfo.Name)
	}
	if res.ProtocolVersion == "" || res.ServerInfo.Version == "" {
		t.Errorf("protocolVersion = %q, version = %q, want both set", res.ProtocolVersion, res.ServerInfo.Version)
	}
	if _, ok := res.Capabilities["tools"]; !ok {
		t.Errorf("capabilities = %v, want the tools capability", res.Capabilities)
	}
}

// TestServePingAndNotification covers the two quiet methods: ping
// answers an empty result, and notifications/initialized — the client's
// own note, no id — earns no answer at all, so the next answer on the
// pipe belongs to the request after it.
func TestServePingAndNotification(t *testing.T) {
	s := start(t, newEnv(t))
	s.notify("notifications/initialized")
	a := s.call(7, "ping", nil)
	if a.Error != nil {
		t.Fatalf("ping: %d %s", a.Error.Code, a.Error.Message)
	}
	if strings.TrimSpace(string(a.Result)) != "{}" {
		t.Errorf("ping result = %s, want {}", a.Result)
	}
}

// TestServeToolsList covers the offer (§12.2): every tool in the table,
// each with a description and a JSON input schema of the object shape.
func TestServeToolsList(t *testing.T) {
	s := start(t, newEnv(t))
	a := s.call(2, "tools/list", nil)
	if a.Error != nil {
		t.Fatalf("tools/list: %d %s", a.Error.Code, a.Error.Message)
	}
	var res struct {
		Tools []toolInfo `json:"tools"`
	}
	if err := json.Unmarshal(a.Result, &res); err != nil {
		t.Fatalf("decode tools/list result %s: %v", a.Result, err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("tool %s inputSchema = %v, want a JSON schema of type object", tool.Name, tool.InputSchema)
		}
	}
	if want := ToolNames(); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("tools/list = %v, want the table's %v", names, want)
	}
}

// TestServeCallStatus covers the status tool against a test engine
// (§12.2): the engine's own state, rendered as text with every status
// counted in its fixed order.
func TestServeCallStatus(t *testing.T) {
	s := start(t, newEnv(t))
	a := s.call(3, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{}})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("status errored: %s", text)
	}
	if !strings.Contains(text, "engine pid ") {
		t.Errorf("status text = %q, want the engine's pid", text)
	}
	for _, status := range engine.JobStatuses {
		if !strings.Contains(text, status+" ") {
			t.Errorf("status text = %q, want %s counted in it", text, status)
		}
	}
	if !strings.Contains(text, "done 1") {
		t.Errorf("status text = %q, want the seeded done job counted", text)
	}
	if !strings.Contains(text, "plans: none") || !strings.Contains(text, "needs you: nothing") {
		t.Errorf("status text = %q, want the empty plans and needs-you sections said in words", text)
	}
	if !strings.Contains(text, "foreman report: none") {
		t.Errorf("status text = %q, want a foreman that never reported said in words", text)
	}
}

// TestServeCallStatusPlansAndNeedsYou covers the status tool's plans,
// report and needs-you sections (§6.1, §6.2, §3, §10): one line per
// plan, its tasks indented beneath it with their branches, the latest
// foreman report with its time and lines, then the queue with each
// entry's reason from its latest event — the same lines the client's
// status command prints.
func TestServeCallStatusPlansAndNeedsYou(t *testing.T) {
	e := newEnv(t)
	err := e.st.Update(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at)
			VALUES (1, 'fix the frobnicator', 'active', ?, ?)`, seedTS, seedTS); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
			VALUES (1, 'the frobnicator', 'the spec', 'go test ./...', 'implement', 'ready', 'main', '{}', ?, ?)`, seedTS, seedTS); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, branch, base_ref, caps, created_at, updated_at)
			VALUES (1, 'wire the sprocket', 'the spec', 'go test ./...', 'implement', 'needs_foreman', 'task/2-1', 'main', '{}', ?, ?)`, seedTS, seedTS); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('task_needs_help', 1, 2, NULL, '{"question":"should the sprocket be blue?"}', ?)`, seedTS); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO events (kind, project_id, task_id, job_id, payload, created_at)
			VALUES ('foreman_report', 1, NULL, NULL, '{"report":"woke for the question of task 2; parked for you\nmerge task/2-1 when approved"}', ?)`, seedTS)
		return err
	})
	if err != nil {
		t.Fatalf("seed the plan and its tasks: %v", err)
	}

	s := start(t, e)
	a := s.call(3, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{}})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("status errored: %s", text)
	}
	for _, want := range []string{
		"plans:\n",
		"#1 active fix the frobnicator\n",
		"  task #1 ready the frobnicator\n",
		"  task #2 needs_foreman wire the sprocket [task/2-1]\n",
		"foreman report:\n",
		seedTS + " project #1: woke for the question of task 2; parked for you\n",
		"  merge task/2-1 when approved\n",
		"needs you:\n",
		"task #2 needs_foreman: should the sprocket be blue?",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status text = %q, want it to contain %q", text, want)
		}
	}
}

// TestServeCallHistory covers the history tool (§9, §12.2): the
// timeline rendered with internal/history, one task's timeline when it
// asks, and a bad argument answered as the tool's own error.
func TestServeCallHistory(t *testing.T) {
	s := start(t, newEnv(t))
	a := s.call(4, "tools/call", map[string]any{"name": "history", "arguments": map[string]any{}})
	text, isError := toolText(t, a)
	if isError {
		t.Fatalf("history errored: %s", text)
	}
	if !strings.Contains(text, "job_completed") || !strings.Contains(text, "answer=ok") {
		t.Errorf("history text = %q, want the event rendered", text)
	}
	if !strings.Contains(text, `{"status":"done","summary":"answered"}`) {
		t.Errorf("history text = %q, want the message rendered", text)
	}

	// The task filter: a task with no timeline says so in words.
	a = s.call(5, "tools/call", map[string]any{"name": "history", "arguments": map[string]any{"task": 99}})
	if text, isError = toolText(t, a); isError || text != "(no history items)" {
		t.Errorf("history of task 99 = %q isError=%v, want the empty page said in words", text, isError)
	}

	// A malformed argument is the tool's answer, not the protocol's.
	a = s.call(6, "tools/call", map[string]any{"name": "history", "arguments": map[string]any{"task": "seven"}})
	if text, isError = toolText(t, a); !isError || !strings.Contains(text, "arguments") {
		t.Errorf("history with a bad argument = %q isError=%v, want the tool's own error", text, isError)
	}
}

// TestServeCallEngineError covers the tool error path (§12.2): an
// engine that refuses comes back as a tool result with isError and the
// engine's message — the bridge itself stays up and answers the next
// call.
func TestServeCallEngineError(t *testing.T) {
	e := newEnv(t)
	// A client whose token the engine does not know: every call is
	// refused, 401 with the engine's own message.
	e.client = api.NewClient(e.socket, "not-a-token")
	s := start(t, e)
	a := s.call(1, "tools/call", map[string]any{"name": "status", "arguments": map[string]any{}})
	text, isError := toolText(t, a)
	if !isError {
		t.Fatalf("status with a refused token = %q, want isError", text)
	}
	if !strings.Contains(text, "missing or unknown token") {
		t.Errorf("error text = %q, want the engine's message in it", text)
	}
	// The bridge survived: the next call still answers.
	if a = s.call(2, "ping", nil); a.Error != nil {
		t.Fatalf("ping after a refused tool call: %d %s", a.Error.Code, a.Error.Message)
	}
}

// TestServeUnknownMethodAndTool covers the refusals (§12.2): a method
// the server has not answers method not found, and tools/call naming a
// tool the bridge has not answers invalid params — never a crash.
func TestServeUnknownMethodAndTool(t *testing.T) {
	s := start(t, newEnv(t))
	a := s.call(1, "resources/list", nil)
	if a.Error == nil || a.Error.Code != codeMethodNotFound {
		t.Errorf("unknown method = %+v, want method not found", a.Error)
	}
	a = s.call(2, "tools/call", map[string]any{"name": "hold_acquire", "arguments": map[string]any{}})
	if a.Error == nil || a.Error.Code != codeInvalidParams || !strings.Contains(a.Error.Message, "hold_acquire") {
		t.Errorf("unknown tool = %+v, want invalid params naming it", a.Error)
	}
}

// TestServeMalformedLine covers the protocol's own floor (§12.2): a
// line that is not JSON-RPC answers a parse error under a null id, and
// the server keeps running for the next line.
func TestServeMalformedLine(t *testing.T) {
	s := start(t, newEnv(t))
	if _, err := s.w.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("write the malformed line: %v", err)
	}
	a := s.recv()
	if a.Error == nil || a.Error.Code != codeParseError {
		t.Fatalf("malformed line = %+v, want a parse error", a.Error)
	}
	if string(a.ID) != "null" {
		t.Errorf("parse error id = %s, want null: a malformed line is nobody's", a.ID)
	}
	if a = s.call(3, "ping", nil); a.Error != nil {
		t.Fatalf("ping after a malformed line: %d %s", a.Error.Code, a.Error.Message)
	}
}

// TestHarnessConfig covers the config the runner writes for a turn
// (§12.2): one server — the named command, run as the mcp bridge over
// the socket — with the token in the environment alone, never in the
// command or its arguments.
func TestHarnessConfig(t *testing.T) {
	data, err := HarnessConfig("/usr/local/bin/brigada", "/run/brigada/api.sock", "lease-secret-9")
	if err != nil {
		t.Fatalf("render the harness config: %v", err)
	}
	var cfg struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode the harness config %s: %v", data, err)
	}
	srv, ok := cfg.Servers[ServerName]
	if !ok {
		t.Fatalf("config servers = %v, want one named %s", cfg.Servers, ServerName)
	}
	if srv.Command != "/usr/local/bin/brigada" {
		t.Errorf("command = %q, want the named binary", srv.Command)
	}
	if strings.Join(srv.Args, " ") != "mcp --socket /run/brigada/api.sock" {
		t.Errorf("args = %v, want the mcp subcommand over the socket", srv.Args)
	}
	if len(srv.Env) != 1 || srv.Env[EnvToken] != "lease-secret-9" {
		t.Errorf("env = %v, want the token under %s alone", srv.Env, EnvToken)
	}
}
