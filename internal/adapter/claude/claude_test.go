package claude

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// No test in this package runs the claude binary: Command is only ever
// built and inspected, and the stream is the fixtures in testdata/.

// stubCLI puts a fake claude executable on PATH for the cases that
// exercise the adapter's default binary: building the command resolves
// the name through PATH, so the stub stands in for the CLI. Nothing
// ever runs it — its exit line is there to make that obvious if it ever
// did.
func stubCLI(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 111\n"), 0o755); err != nil {
		t.Fatalf("write the CLI stub: %v", err)
	}
	t.Setenv("PATH", dir)
}

func TestName(t *testing.T) {
	if got := (&Adapter{}).Name(); got != "claude" {
		t.Errorf("Name() = %q, want %q", got, "claude")
	}
}

func TestCommand(t *testing.T) {
	stubCLI(t)
	const prompt = "a long, private worker prompt with\nthe task spec verbatim"
	dir := t.TempDir()
	cases := []struct {
		name    string
		ad      Adapter
		job     adapter.Job
		resume  string
		wantExe string
		wantArg []string // the arguments after the executable
	}{
		{
			name:    "fresh run with model",
			ad:      Adapter{Binary: "/usr/local/bin/claude"},
			job:     adapter.Job{Prompt: prompt, Model: "sonnet"},
			wantExe: "/usr/local/bin/claude",
			wantArg: []string{"-p", "--output-format", "stream-json", "--verbose",
				"--model", "sonnet", "--permission-mode", "acceptEdits"},
		},
		{
			name:    "resume run, default binary, no model",
			job:     adapter.Job{Prompt: prompt},
			resume:  "sess-9",
			wantExe: "claude",
			wantArg: []string{"-p", "--output-format", "stream-json", "--verbose",
				"--resume", "sess-9", "--permission-mode", "acceptEdits"},
		},
		{
			name: "tool scope and permission mode pass through",
			ad: Adapter{
				PermissionMode:  "bypassPermissions",
				AllowedTools:    []string{"Bash(git:*)", "Edit"},
				DisallowedTools: []string{"WebFetch"},
			},
			job:     adapter.Job{Prompt: prompt},
			wantExe: "claude",
			wantArg: []string{"-p", "--output-format", "stream-json", "--verbose",
				"--permission-mode", "bypassPermissions",
				"--allowedTools", "Bash(git:*)", "Edit",
				"--disallowedTools", "WebFetch"},
		},
	}
	for _, tc := range cases {
		cmd, err := tc.ad.Command(tc.job, dir, tc.resume)
		if err != nil {
			t.Fatalf("%s: build command: %v", tc.name, err)
		}
		if cmd.Dir != dir {
			t.Errorf("%s: working directory = %q, want the worktree %q", tc.name, cmd.Dir, dir)
		}
		if got := append([]string{tc.wantExe}, tc.wantArg...); !slices.Equal(cmd.Args, got) {
			t.Errorf("%s: args = %v\nwant %v", tc.name, cmd.Args, got)
		}
		// The prompt rides on stdin, never argv: the command line is
		// public to anything listing processes.
		if cmd.Stdin == nil {
			t.Fatalf("%s: the command has no stdin", tc.name)
		}
		gotPrompt, err := io.ReadAll(cmd.Stdin)
		if err != nil {
			t.Fatalf("%s: read the command's stdin: %v", tc.name, err)
		}
		if string(gotPrompt) != tc.job.Prompt {
			t.Errorf("%s: stdin = %q, want the prompt", tc.name, gotPrompt)
		}
		if rendered := cmd.String(); strings.Contains(rendered, tc.job.Prompt) {
			t.Errorf("%s: the command renders its prompt: %s", tc.name, rendered)
		}
		for _, arg := range cmd.Args {
			if strings.Contains(arg, "private worker prompt") {
				t.Errorf("%s: argv carries the prompt: %v", tc.name, cmd.Args)
			}
		}
	}
}

// TestCommandEnvIsTheRunnersMinusBrigada covers the environment policy
// (§5.1): the worker inherits the machine's and the CLI's own login
// state, but not one byte of brigada's — the runner's token, the
// engine's socket, anything BRIGADA-prefixed never reaches the worker.
func TestCommandEnvIsTheRunnersMinusBrigada(t *testing.T) {
	stubCLI(t)
	t.Setenv("BRIGADA_RUNNER_TOKEN", "runner-secret-9")
	t.Setenv("BRIGADA_ENGINE_SOCKET", "/run/brigada/api.sock")
	t.Setenv("BRIGADA_TOKEN", "engine-secret-9")
	t.Setenv("CLAUDE_CODE_AMBIENT", "keep-me")

	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	if len(cmd.Env) == 0 {
		t.Fatal("the command has no environment")
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "BRIGADA_") {
			t.Errorf("brigada's own variable reached the worker: %s", kv)
		}
		if strings.Contains(kv, "runner-secret-9") || strings.Contains(kv, "engine-secret-9") {
			t.Errorf("a secret value reached the worker: %s", kv)
		}
	}
	if !slices.Contains(cmd.Env, "CLAUDE_CODE_AMBIENT=keep-me") {
		t.Error("the ambient environment did not survive the scrub")
	}
}

func TestParseEvent(t *testing.T) {
	a := &Adapter{}
	cases := []struct {
		name string
		line string
		want adapter.Event
		ok   bool
	}{
		{"init session", `{"type":"system","subtype":"init","session_id":"sess-7"}`,
			adapter.Event{Kind: adapter.EvSession, SessionID: "sess-7"}, true},
		{"init without session id", `{"type":"system","subtype":"init"}`, adapter.Event{}, false},
		{"system other than init", `{"type":"system","subtype":"compact_border"}`, adapter.Event{}, false},
		{"assistant text", `{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}`,
			adapter.Event{Kind: adapter.EvMessage, Text: "working on it"}, true},
		// A message that calls a tool is the tool call, whatever prose
		// rides beside it.
		{"assistant tool use wins", `{"type":"assistant","message":{"content":[{"type":"text","text":"let me look"},{"type":"tool_use","name":"Read","id":"toolu-1"}]}}`,
			adapter.Event{Kind: adapter.EvToolStart, Text: "Read"}, true},
		{"assistant without content", `{"type":"assistant","message":{"role":"assistant"}}`, adapter.Event{}, false},
		{"tool result ends by id", `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu-1","is_error":false}]}}`,
			adapter.Event{Kind: adapter.EvToolEnd, Text: "toolu-1"}, true},
		// A user line speaking text is the CLI's echo of input, not the
		// worker's progress.
		{"user text is not progress", `{"type":"user","message":{"content":[{"type":"text","text":"the prompt"}]}}`,
			adapter.Event{}, false},
		{"result success carries session and text", `{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"sess-7"}`,
			adapter.Event{Kind: adapter.EvSession, SessionID: "sess-7", Text: "all done"}, true},
		{"result success without session id", `{"type":"result","subtype":"success","result":"all done"}`,
			adapter.Event{Kind: adapter.EvMessage, Text: "all done"}, true},
		{"error result by is_error only", `{"type":"result","is_error":true,"result":"the CLI gave up"}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: "the CLI gave up"}, true},
		{"error result names its subtype when silent", `{"type":"result","subtype":"error_max_turns","is_error":true}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: "error_max_turns"}, true},
		{"limit result, mixed case", `{"type":"result","subtype":"error_limit","is_error":true,"result":"Usage LIMIT reached"}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit, Text: "Usage LIMIT reached"}, true},
		{"auth result", `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Not logged in: please run /login"}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth, Text: "Not logged in: please run /login"}, true},
		{"resume result", `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"No conversation found with session ID: x"}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrResumeFailed, Text: "No conversation found with session ID: x"}, true},
		// The CLI also puts failures on stderr as plain text; on stdout
		// prose the broad table still reads them, the error result's own
		// vocabulary.
		{"plain-text limit line", `⏺ You've hit your usage limit. Try again at 9pm.`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit, Text: `⏺ You've hit your usage limit. Try again at 9pm.`}, true},
		{"plain-text auth line", `error: unauthorized (401)`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth, Text: `error: unauthorized (401)`}, true},
		{"plain-text resume line", `No session found: conversation not found for sess-9`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrResumeFailed, Text: `No session found: conversation not found for sess-9`}, true},
		{"banner prose", `claude-code 2.0.1 — starting session`, adapter.Event{}, false},
		{"blank line", `   `, adapter.Event{}, false},
		{"unknown type", `{"type":"stream_event","data":"…"}`, adapter.Event{}, false},
		{"not json", `oops`, adapter.Event{}, false},
		{"broken json", `{"type":"assistant","message":`, adapter.Event{}, false},
		{"json that is not an object", `[{"type":"assistant"}]`, adapter.Event{}, false},
	}
	for _, tc := range cases {
		got, ok := a.ParseEvent([]byte(tc.line))
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: event = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestParseErrEvent(t *testing.T) {
	a := &Adapter{}
	cases := []struct {
		name string
		line string
		want adapter.Event
		ok   bool
	}{
		// The failures the CLI prints only to stderr, before any JSON:
		// plain text named in full, sorted by the strict table — stderr
		// also carries the CLI's warnings, so only a phrase that names
		// the failure itself counts there.
		{"resume failure", `No conversation found with session ID: sess-gone`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrResumeFailed, Text: `No conversation found with session ID: sess-gone`}, true},
		{"limit, mixed case", `⏺ Usage LIMIT reached — resets at 9pm`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit, Text: `⏺ Usage LIMIT reached — resets at 9pm`}, true},
		{"auth, not logged in", `error: not logged in`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth, Text: `error: not logged in`}, true},
		{"auth, invalid key", `Invalid API key provided`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth, Text: `Invalid API key provided`}, true},
		// A JSON error line parses as it would on stdout: an error
		// result by the broad table, an error object by the strict one.
		{"json error result", `{"type":"result","subtype":"error_limit","is_error":true,"result":"Usage limit reached"}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit, Text: "Usage limit reached"}, true},
		{"json error naming no known failure", `{"type":"result","is_error":true,"result":"the CLI gave up"}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: "the CLI gave up"}, true},
		{"json error object", `{"type":"error","error":{"message":"Usage limit reached — resets at 9pm"}}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit,
				Text: `{"type":"error","error":{"message":"Usage limit reached — resets at 9pm"}}`}, true},
		{"json error object naming no failure", `{"type":"error","error":{"message":"MCP server requires authentication"}}`,
			adapter.Event{}, false},
		// Stderr carries failures only: the CLI's other events belong to
		// its stdout stream, and a line there is not one of them.
		{"json success on stderr", `{"type":"result","subtype":"success","result":"all done","session_id":"sess-7"}`,
			adapter.Event{}, false},
		{"json assistant on stderr", `{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}`,
			adapter.Event{}, false},
		// Warnings and banners carry nothing to act on: the broad
		// substrings — "authentication", "rate limit", "unauthorized" —
		// are exactly what a harmless stderr line mentions.
		{"mcp server requires authentication", `Warning: MCP server "github" requires authentication; its tools are unavailable`,
			adapter.Event{}, false},
		{"rate-limited retry", `⏳ Rate limited — retrying in 2s (attempt 2/5)`, adapter.Event{}, false},
		{"unauthorized telemetry note", `debug: telemetry push unauthorized (401); skipped`, adapter.Event{}, false},
		{"old limit wording is not the failure", `⏺ You've hit your usage limit. Try again at 9pm.`, adapter.Event{}, false},
		{"unknown session option", `Warning: unknown session option 'strict' ignored`, adapter.Event{}, false},
		{"config file not found", `debug: config file not found, using defaults`, adapter.Event{}, false},
		{"banner", `claude-code 2.0.1 — starting headless session`, adapter.Event{}, false},
		{"blank line", `   `, adapter.Event{}, false},
		{"not json", `oops`, adapter.Event{}, false},
		{"broken json", `{"type":"assistant","message":`, adapter.Event{}, false},
	}
	for _, tc := range cases {
		got, ok := a.ParseErrEvent([]byte(tc.line))
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: event = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestStderrFixtures reads the CLI's stderr transcripts from testdata/:
// the failure each names is the event it yields, however much noise
// rides around it, and noise alone yields nothing.
func TestStderrFixtures(t *testing.T) {
	a := &Adapter{}
	cases := []struct {
		name    string
		fixture string
		events  []adapter.Event
	}{
		{
			name:    "a resume that finds no conversation",
			fixture: "stderr-resume.txt",
			events: []adapter.Event{
				{Kind: adapter.EvError, ErrorKind: adapter.ErrResumeFailed,
					Text: "No conversation found with session ID: sess-gone. Please check the session ID and try again."},
			},
		},
		{
			name:    "a limit hit",
			fixture: "stderr-limit.txt",
			events: []adapter.Event{
				{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit,
					Text: "⏺ Usage limit reached — resets at 9pm."},
			},
		},
		{
			name:    "an auth failure",
			fixture: "stderr-auth.txt",
			events: []adapter.Event{
				{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth,
					Text: "error: Invalid API key provided. Please run /login to authenticate, then try again."},
			},
		},
		{
			// Warnings alone — the harmless lines that mention
			// authentication and rate limits among them — yield nothing.
			name:    "warnings only, auth and rate-limit mentions included",
			fixture: "stderr-noise.txt",
		},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		var events []adapter.Event
		for _, line := range strings.Split(string(data), "\n") {
			if ev, ok := a.ParseErrEvent([]byte(line)); ok {
				events = append(events, ev)
			}
		}
		if !slices.Equal(events, tc.events) {
			t.Errorf("%s: events =\n%+v\nwant\n%+v", tc.name, events, tc.events)
		}
	}
}

// parseFixture reads one fixture transcript and returns the events its
// lines produce, in order. The lines that are not events are simply
// absent — which is what the fixtures' malformed lines are there to
// prove.
func parseFixture(t *testing.T, a *Adapter, name string) []adapter.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var events []adapter.Event
	for _, line := range strings.Split(string(data), "\n") {
		if ev, ok := a.ParseEvent([]byte(line)); ok {
			events = append(events, ev)
		}
	}
	return events
}

// lastSession is the id the runner would write back (§4.2): the last
// session the stream carried.
func lastSession(events []adapter.Event) string {
	id := ""
	for _, e := range events {
		if e.Kind == adapter.EvSession && e.SessionID != "" {
			id = e.SessionID
		}
	}
	return id
}

func TestTranscripts(t *testing.T) {
	a := &Adapter{}
	const normalResult = "Added work.txt.\n\n```json\n{\"status\": \"done\", \"summary\": \"added work.txt\", \"branch\": \"task-7-1\"}\n```"
	const forkedResult = "Applied the review's change.\n\n{\"status\": \"done\", \"summary\": \"applied the review change\"}"
	cases := []struct {
		name       string
		fixture    string
		events     []adapter.Event
		session    string
		resultText string
	}{
		{
			name:    "a normal run with tool calls, malformed lines interleaved",
			fixture: "normal.jsonl",
			events: []adapter.Event{
				{Kind: adapter.EvSession, SessionID: "sess-1"},
				{Kind: adapter.EvMessage, Text: "Reading the task, then adding the file."},
				{Kind: adapter.EvToolStart, Text: "Read"},
				{Kind: adapter.EvToolEnd, Text: "toolu-1"},
				{Kind: adapter.EvToolStart, Text: "Edit"},
				{Kind: adapter.EvToolEnd, Text: "toolu-2"},
				{Kind: adapter.EvMessage, Text: "Added work.txt."},
				{Kind: adapter.EvSession, SessionID: "sess-1", Text: normalResult},
			},
			session:    "sess-1",
			resultText: normalResult,
		},
		{
			name:    "a limit hit",
			fixture: "limit.jsonl",
			events: []adapter.Event{
				{Kind: adapter.EvSession, SessionID: "sess-2"},
				{Kind: adapter.EvMessage, Text: "Working on the fix."},
				{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit, Text: "You've hit your usage limit. Try again at 9pm."},
			},
			session: "sess-2",
			// No result line came; the last assistant message stands in.
			resultText: "Working on the fix.",
		},
		{
			name:    "an auth failure",
			fixture: "auth.jsonl",
			events: []adapter.Event{
				{Kind: adapter.EvSession, SessionID: "sess-3"},
				{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth, Text: "Invalid API key provided. Please run /login to authenticate, then try again."},
			},
			session: "sess-3",
		},
		{
			// A resume forks the session id: the id the run ends with is
			// the one the next round must resume (§4.2), not the one it
			// started from.
			name:    "a resume that forks the session id",
			fixture: "resume-fork.jsonl",
			events: []adapter.Event{
				{Kind: adapter.EvSession, SessionID: "sess-first"},
				{Kind: adapter.EvMessage, Text: "Continuing the task with the review feedback."},
				{Kind: adapter.EvToolStart, Text: "Edit"},
				{Kind: adapter.EvToolEnd, Text: "toolu-7"},
				{Kind: adapter.EvSession, SessionID: "sess-forked", Text: forkedResult},
			},
			session:    "sess-forked",
			resultText: forkedResult,
		},
		{
			name:    "a resume that finds no session",
			fixture: "resume-not-found.jsonl",
			events: []adapter.Event{
				{Kind: adapter.EvError, ErrorKind: adapter.ErrResumeFailed,
					Text: "No conversation found with session ID: sess-gone. Please check the session ID and try again."},
			},
			session: "",
		},
	}
	for _, tc := range cases {
		events := parseFixture(t, a, tc.fixture)
		if !slices.Equal(events, tc.events) {
			t.Errorf("%s: events =\n%+v\nwant\n%+v", tc.name, events, tc.events)
		}
		if got := lastSession(events); got != tc.session {
			t.Errorf("%s: session id = %q, want %q", tc.name, got, tc.session)
		}
		if got := a.ResultText(events); got != tc.resultText {
			t.Errorf("%s: ResultText = %q, want %q", tc.name, got, tc.resultText)
		}
	}
}

// TestResultTextNoEvents covers the stream that died saying nothing.
func TestResultTextNoEvents(t *testing.T) {
	if got := (&Adapter{}).ResultText(nil); got != "" {
		t.Errorf("ResultText(nil) = %q, want empty", got)
	}
}
