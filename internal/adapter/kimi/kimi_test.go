package kimi

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// No test in this package runs the kimi binary: Command is only ever
// built and inspected, and the stream is the fixtures in testdata/.

// stubCLI puts a fake kimi executable on PATH for the cases that
// exercise the adapter's default binary: building the command resolves
// the name through PATH, so the stub stands in for the CLI. Nothing
// ever runs it — its exit line is there to make that obvious if it ever
// did.
func stubCLI(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kimi"), []byte("#!/bin/sh\nexit 111\n"), 0o755); err != nil {
		t.Fatalf("write the CLI stub: %v", err)
	}
	t.Setenv("PATH", dir)
}

func TestName(t *testing.T) {
	if got := (&Adapter{}).Name(); got != "kimi" {
		t.Errorf("Name() = %q, want %q", got, "kimi")
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
			name:    "fresh run",
			job:     adapter.Job{Prompt: prompt},
			wantExe: "kimi",
			wantArg: []string{"-p", prompt, "--output-format", "stream-json"},
		},
		{
			name:    "fresh run with model and agent file",
			ad:      Adapter{Binary: "/usr/local/bin/kimi", AgentFile: "/agents/worker.md"},
			job:     adapter.Job{Prompt: prompt, Model: "k2"},
			wantExe: "/usr/local/bin/kimi",
			wantArg: []string{"-p", prompt, "--output-format", "stream-json",
				"-m", "k2", "--agent-file", "/agents/worker.md"},
		},
		{
			// The CLI refuses --agent-file beside --session, so a resumed
			// session carries no agent file: it keeps the agent it was
			// created with.
			name:    "resume keeps no agent file",
			ad:      Adapter{AgentFile: "/agents/worker.md"},
			job:     adapter.Job{Prompt: prompt},
			resume:  "session_9f2a",
			wantExe: "kimi",
			wantArg: []string{"-p", prompt, "--output-format", "stream-json",
				"--session", "session_9f2a"},
		},
		{
			name:    "extra args pass through",
			ad:      Adapter{ExtraArgs: []string{"--lang", "en"}},
			job:     adapter.Job{Prompt: prompt},
			wantExe: "kimi",
			wantArg: []string{"-p", prompt, "--output-format", "stream-json", "--lang", "en"},
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
			t.Errorf("%s: args =\n%v\nwant\n%v", tc.name, cmd.Args, got)
		}
	}
}

// TestCommandMCPConfigRefused covers the adapter's one refusal: kimi
// has no MCP-config flag, so a job carrying one — a foreman turn with
// the `brigada mcp` bridge (§12.2) — cannot run here. Refusing beats
// silently dropping the bridge the turn needs, fresh or resumed.
func TestCommandMCPConfigRefused(t *testing.T) {
	job := adapter.Job{Prompt: "p", MCPConfig: "/runner-state/mcp/turn-x1/config.json"}
	for _, resume := range []string{"", "session_9f2a"} {
		if _, err := (&Adapter{}).Command(job, t.TempDir(), resume); err == nil {
			t.Errorf("resume %q: a job with an MCP config built a command", resume)
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
	t.Setenv("KIMI_CODE_AMBIENT", "keep-me")

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
		if strings.Contains(kv, "runner-secret-9") {
			t.Errorf("a secret value reached the worker: %s", kv)
		}
	}
	if !slices.Contains(cmd.Env, "KIMI_CODE_AMBIENT=keep-me") {
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
		{"tool call starts", `{"role":"assistant","tool_calls":[{"type":"function","id":"tool_a","function":{"name":"Read","arguments":"{\"path\":\"README.md\"}"}}]}`,
			adapter.Event{Kind: adapter.EvToolStart, Text: "Read"}, true},
		// A line that both speaks and calls tools is its tool call: the
		// prose riding beside a call is mid-turn narration, which
		// ResultText never reads as the answer.
		{"text beside a call yields the call", `{"role":"assistant","content":"Now let me look at the tests:","tool_calls":[{"type":"function","id":"tool_b","function":{"name":"Grep","arguments":"{}"}}]}`,
			adapter.Event{Kind: adapter.EvToolStart, Text: "Grep"}, true},
		{"tool result ends by id", `{"role":"tool","tool_call_id":"tool_a","content":"1\t# the repository"}`,
			adapter.Event{Kind: adapter.EvToolEnd, Text: "tool_a"}, true},
		{"tool result with parts content", `{"role":"tool","tool_call_id":"tool_b","content":[{"type":"text","text":"two matches"}]}`,
			adapter.Event{Kind: adapter.EvToolEnd, Text: "tool_b"}, true},
		{"assistant text", `{"role":"assistant","content":"The final answer text."}`,
			adapter.Event{Kind: adapter.EvMessage, Text: "The final answer text."}, true},
		{"assistant text as parts, non-text parts skipped", `{"role":"assistant","content":[{"type":"image","source":"…"},{"type":"text","text":"a"},{"type":"text","text":"b"}]}`,
			adapter.Event{Kind: adapter.EvMessage, Text: "a\nb"}, true},
		{"assistant saying nothing", `{"role":"assistant","content":""}`, adapter.Event{}, false},
		{"resume hint carries the session", `{"role":"meta","type":"session.resume_hint","session_id":"session_1f08abcd","command":"kimi -r session_1f08abcd","content":"To resume this session: kimi -r session_1f08abcd"}`,
			adapter.Event{Kind: adapter.EvSession, SessionID: "session_1f08abcd"}, true},
		{"resume hint without a session id", `{"role":"meta","type":"session.resume_hint","content":"To resume this session: …"}`, adapter.Event{}, false},
		{"version banner", `{"role":"meta","type":"system.version","version":"2.1.0"}`, adapter.Event{}, false},
		{"unknown meta type", `{"role":"meta","type":"system.info","info":"…"}`, adapter.Event{}, false},
		{"unknown role", `{"role":"user","content":"the prompt"}`, adapter.Event{}, false},
		// The CLI's failures are plain lines, on stdout as on stderr:
		// an `error: failed to run prompt:` line is an error event
		// sorted by its reason, and the noise around it is not.
		{"resume failure", `error: failed to run prompt: Session "session_0eaf12cd" not found.`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrResumeFailed,
				Text: `error: failed to run prompt: Session "session_0eaf12cd" not found.`}, true},
		{"auth failure, invalid grant", `error: failed to run prompt: The provided authorization grant is invalid`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth,
				Text: "error: failed to run prompt: The provided authorization grant is invalid"}, true},
		{"auth failure, no credential", `error: failed to run prompt: provider managed:kimi-code has no credential configured`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth,
				Text: "error: failed to run prompt: provider managed:kimi-code has no credential configured"}, true},
		{"auth failure, unauthorized", `error: failed to run prompt: unauthorized (401)`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth,
				Text: "error: failed to run prompt: unauthorized (401)"}, true},
		// No real kimi limit message has been captured; the phrasing the
		// table carries is expected vocabulary, nothing more.
		{"limit failure, unverified phrasing", `error: failed to run prompt: usage limit reached, try again later`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit,
				Text: "error: failed to run prompt: usage limit reached, try again later"}, true},
		{"failure naming no known kind", `error: failed to run prompt: the model endpoint vanished`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther,
				Text: "error: failed to run prompt: the model endpoint vanished"}, true},
		{"log hint carries nothing", `See log: /home/worker/.kimi-code/logs/kimi-code.log`, adapter.Event{}, false},
		{"telemetry note carries nothing", `telemetry disabled; unset KIMI_TELEMETRY to enable`, adapter.Event{}, false},
		{"banner prose", `kimi-code 2.1.0 — starting session`, adapter.Event{}, false},
		{"blank line", `   `, adapter.Event{}, false},
		{"not json", `oops`, adapter.Event{}, false},
		{"broken json", `{"role":"assistant","tool_calls":`, adapter.Event{}, false},
		{"json that is not an object", `[{"role":"assistant"}]`, adapter.Event{}, false},
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

// TestParseErrEvent covers the other stream: the CLI prints its
// failures to either one in the same shapes, so stderr sorts every
// line exactly as stdout does — the same failures become error events,
// and the same noise stays nothing.
func TestParseErrEvent(t *testing.T) {
	a := &Adapter{}
	cases := []struct {
		name string
		line string
		want adapter.Event
		ok   bool
	}{
		{"resume failure", `error: failed to run prompt: Session "session_0eaf12cd" not found.`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrResumeFailed,
				Text: `error: failed to run prompt: Session "session_0eaf12cd" not found.`}, true},
		{"auth failure", `error: failed to run prompt: The provided authorization grant is invalid`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth,
				Text: "error: failed to run prompt: The provided authorization grant is invalid"}, true},
		{"limit failure, unverified phrasing", `Error: failed to run prompt: rate limit exceeded, retry at 10:00`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit,
				Text: "Error: failed to run prompt: rate limit exceeded, retry at 10:00"}, true},
		{"failure naming no known kind", `error: failed to run prompt: the model endpoint vanished`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther,
				Text: "error: failed to run prompt: the model endpoint vanished"}, true},
		{"json failure line sorts the same", `{"role":"assistant","content":"partial work before the crash"}`,
			adapter.Event{Kind: adapter.EvMessage, Text: "partial work before the crash"}, true},
		{"log hint carries nothing", `See log: /home/worker/.kimi-code/logs/kimi-code.log`, adapter.Event{}, false},
		{"telemetry note carries nothing", `telemetry disabled; unset KIMI_TELEMETRY to enable`, adapter.Event{}, false},
		{"warning", `Warning: config file not found, using defaults`, adapter.Event{}, false},
		{"blank line", `   `, adapter.Event{}, false},
		{"not json", `oops`, adapter.Event{}, false},
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

// TestFailureFixtures reads the CLI's failure transcripts from
// testdata/: the failure each names is the one event it yields, on
// either stream, however much noise rides around it.
func TestFailureFixtures(t *testing.T) {
	a := &Adapter{}
	cases := []struct {
		name    string
		fixture string
		event   adapter.Event
	}{
		{
			name:    "a resume that finds no session",
			fixture: "failure-resume.txt",
			event: adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrResumeFailed,
				Text: `error: failed to run prompt: Session "session_0eaf12cd" not found.`},
		},
		{
			name:    "an invalid authorization grant",
			fixture: "failure-auth.txt",
			event: adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth,
				Text: "error: failed to run prompt: The provided authorization grant is invalid"},
		},
		{
			name:    "no credential configured",
			fixture: "failure-credential.txt",
			event: adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth,
				Text: "error: failed to run prompt: provider managed:kimi-code has no credential configured"},
		},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		// Both streams: the CLI prints its failures to either, and the
		// adapter reads them alike.
		for _, parse := range []func([]byte) (adapter.Event, bool){a.ParseEvent, a.ParseErrEvent} {
			var events []adapter.Event
			for _, line := range strings.Split(string(data), "\n") {
				if ev, ok := parse([]byte(line)); ok {
					events = append(events, ev)
				}
			}
			if !slices.Equal(events, []adapter.Event{tc.event}) {
				t.Errorf("%s: events =\n%+v\nwant\n%+v", tc.name, events, []adapter.Event{tc.event})
			}
		}
	}
}

// parseFixture reads one fixture transcript and returns the events its
// lines produce, in order. The lines that are not events are simply
// absent.
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
	const finalParts = "The work is complete.\nSummary attached."
	cases := []struct {
		name       string
		fixture    string
		events     []adapter.Event
		session    string
		resultText string
	}{
		{
			name:    "a normal run with tool calls",
			fixture: "normal.jsonl",
			events: []adapter.Event{
				{Kind: adapter.EvToolStart, Text: "Read"},
				{Kind: adapter.EvToolEnd, Text: "tool_a"},
				// The mid-turn text riding beside the Grep call never
				// became a message; only the answer after the last tool
				// call did.
				{Kind: adapter.EvToolStart, Text: "Grep"},
				{Kind: adapter.EvToolEnd, Text: "tool_b"},
				{Kind: adapter.EvMessage, Text: "The final answer text."},
				{Kind: adapter.EvSession, SessionID: "session_1f08abcd"},
			},
			session:    "session_1f08abcd",
			resultText: "The final answer text.",
		},
		{
			// The answer arrives as a list of text parts, and a message
			// that only speaks before later tool calls is mid-turn: the
			// tool calls that follow retire it.
			name:    "the answer in parts, after mid-turn messages",
			fixture: "parts.jsonl",
			events: []adapter.Event{
				{Kind: adapter.EvMessage, Text: "I will inspect the repository first."},
				{Kind: adapter.EvToolStart, Text: "Glob"},
				{Kind: adapter.EvToolEnd, Text: "tool_c"},
				{Kind: adapter.EvToolStart, Text: "Grep"},
				{Kind: adapter.EvToolEnd, Text: "tool_d"},
				{Kind: adapter.EvMessage, Text: finalParts},
				{Kind: adapter.EvSession, SessionID: "session_2b91cdef"},
			},
			session:    "session_2b91cdef",
			resultText: finalParts,
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

// TestResultText covers the answer's rule directly: only text after
// the final tool call counts, and a stream with none has none.
func TestResultText(t *testing.T) {
	cases := []struct {
		name   string
		events []adapter.Event
		want   string
	}{
		{"no events", nil, ""},
		{"only tools", []adapter.Event{
			{Kind: adapter.EvToolStart, Text: "Read"},
			{Kind: adapter.EvToolEnd, Text: "tool_a"},
		}, ""},
		{"a message before the last tool call is mid-turn", []adapter.Event{
			{Kind: adapter.EvMessage, Text: "let me check the file"},
			{Kind: adapter.EvToolStart, Text: "Read"},
			{Kind: adapter.EvToolEnd, Text: "tool_a"},
		}, ""},
		{"the last message after the final tool call wins", []adapter.Event{
			{Kind: adapter.EvMessage, Text: "let me check the file"},
			{Kind: adapter.EvToolStart, Text: "Read"},
			{Kind: adapter.EvToolEnd, Text: "tool_a"},
			{Kind: adapter.EvMessage, Text: "first draft"},
			{Kind: adapter.EvMessage, Text: "the answer"},
		}, "the answer"},
	}
	for _, tc := range cases {
		if got := (&Adapter{}).ResultText(tc.events); got != tc.want {
			t.Errorf("%s: ResultText = %q, want %q", tc.name, got, tc.want)
		}
	}
}
