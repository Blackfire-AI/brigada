package codex

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// No test in this package runs the codex binary: Command is only ever
// built and inspected, and the stream is the fixtures in testdata/.

// stubCLI puts a fake codex executable on PATH; nothing ever runs it.
func stubCLI(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nexit 111\n"), 0o755); err != nil {
		t.Fatalf("write the CLI stub: %v", err)
	}
	t.Setenv("PATH", dir)
}

func TestName(t *testing.T) {
	if got := (&Adapter{}).Name(); got != "codex" {
		t.Errorf("Name() = %q, want %q", got, "codex")
	}
}

func TestForemanHarnessEligibility(t *testing.T) {
	if !adapter.ResumeCapable("codex") {
		t.Error("codex is absent from ResumeCapableHarnesses")
	}
}

func TestCommand(t *testing.T) {
	stubCLI(t)
	const prompt = "--a private worker prompt with\nthe task spec verbatim"
	dir := t.TempDir()
	base := []string{"exec", "--json", "-s", "workspace-write"}
	cases := []struct {
		name    string
		ad      Adapter
		job     adapter.Job
		resume  string
		wantExe string
		wantArg []string
	}{
		{
			name:    "fresh run",
			job:     adapter.Job{Prompt: prompt},
			wantExe: "codex",
			wantArg: append(slices.Clone(base), "-"),
		},
		{
			name:    "fresh run with model",
			ad:      Adapter{Binary: "/usr/local/bin/codex"},
			job:     adapter.Job{Prompt: prompt, Model: "gpt-5"},
			wantExe: "/usr/local/bin/codex",
			wantArg: append(slices.Clone(base), "-m", "gpt-5", "-"),
		},
		{
			name:    "resume names the thread",
			job:     adapter.Job{Prompt: prompt},
			resume:  "01a140b7-6c27-7441-b9a1-1a082df4d4c9",
			wantExe: "codex",
			wantArg: append(slices.Clone(base), "resume", "01a140b7-6c27-7441-b9a1-1a082df4d4c9", "-"),
		},
		{
			name:    "the default model names none",
			job:     adapter.Job{Prompt: prompt, Model: DefaultModel},
			wantExe: "codex",
			wantArg: append(slices.Clone(base), "-"),
		},
		{
			name:    "extra args pass through",
			ad:      Adapter{ExtraArgs: []string{"-c", "x=1"}},
			job:     adapter.Job{Prompt: prompt},
			wantExe: "codex",
			wantArg: append(slices.Clone(base), "-c", "x=1", "-"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.ad.Command(tc.job, dir, tc.resume)
			if err != nil {
				t.Fatalf("Command: %v", err)
			}
			if filepath.Base(cmd.Path) != filepath.Base(tc.wantExe) {
				t.Errorf("executable = %q, want %q", cmd.Path, tc.wantExe)
			}
			if got := cmd.Args[1:]; !slices.Equal(got, tc.wantArg) {
				t.Errorf("args = %q, want %q", got, tc.wantArg)
			}
			if cmd.Dir != dir {
				t.Errorf("Dir = %q, want %q", cmd.Dir, dir)
			}
			for _, a := range cmd.Args {
				if strings.Contains(a, "dangerously") {
					t.Errorf("arg %q bypasses the sandbox", a)
				}
			}
		})
	}
}

func TestCommandMCPConfig(t *testing.T) {
	stubCLI(t)
	config := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(config, []byte(`{"mcpServers":{"brigada":{"command":"/bin/brigada","args":["mcp","--socket","/tmp/sock"],"env":{"BRIGADA_TOKEN":"secret"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", MCPConfig: config, Model: "gpt-5"}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	args := strings.Join(cmd.Args[1:], " ")
	if strings.Contains(args, "--approve-for-me") || !strings.Contains(args, "--skip-git-repo-check") || !strings.Contains(args, "-s read-only") || !strings.Contains(args, `approvals_reviewer="auto_review"`) || !strings.Contains(args, "sandbox_approval=false") || !strings.Contains(args, "request_permissions=false") || !strings.Contains(args, "mcp_elicitations=true") || !strings.Contains(args, `mcp_servers.brigada.command="/bin/brigada"`) || !strings.Contains(args, `mcp_servers.brigada.args=["mcp","--socket","/tmp/sock"]`) || !strings.Contains(args, `mcp_servers.brigada.env_vars=["BRIGADA_TOKEN"]`) {
		t.Errorf("foreman args = %q", args)
	}
	if strings.Contains(args, "secret") {
		t.Errorf("the token is in argv: %q", args)
	}
	if !slices.Contains(cmd.Env, "BRIGADA_TOKEN=secret") {
		t.Errorf("the token is not in the process env")
	}
}

func TestCommandPromptOnStdin(t *testing.T) {
	stubCLI(t)
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "the private prompt"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "private") {
		t.Errorf("prompt in argv: %q", cmd.Args)
	}
	b, _ := io.ReadAll(cmd.Stdin)
	if string(b) != "the private prompt" {
		t.Errorf("stdin = %q", b)
	}
}

func TestCommandReviewIsReadOnly(t *testing.T) {
	stubCLI(t)
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Review: true}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "-s read-only") || strings.Contains(args, "workspace-write") {
		t.Errorf("review args = %q", args)
	}
}

func TestCommandMCPConfigRejectsInvalidServers(t *testing.T) {
	stubCLI(t)
	for name, body := range map[string]string{
		"two servers":  `{"mcpServers":{"a":{"command":"a","args":[],"env":{}},"b":{"command":"b","args":[],"env":{}}}}`,
		"missing args": `{"mcpServers":{"brigada":{"command":"/bin/brigada","env":{}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", MCPConfig: path}, t.TempDir(), ""); err == nil {
				t.Fatal("Command accepted an invalid MCP config")
			}
		})
	}
}

func TestCommandEnvIsTheRunnersMinusBrigada(t *testing.T) {
	stubCLI(t)
	t.Setenv("BRIGADA_RUNNER_TOKEN", "secret")
	t.Setenv("CODEX_HOME", "/home/x/.codex")
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	var kept bool
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "BRIGADA_") {
			t.Errorf("env carries %q", kv)
		}
		if kv == "CODEX_HOME=/home/x/.codex" {
			kept = true
		}
	}
	if !kept {
		t.Error("env lost the CLI's own variables")
	}
}

func parseFixture(t *testing.T, name string) []adapter.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	a := &Adapter{}
	var events []adapter.Event
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if ev, ok := a.ParseEvent(sc.Bytes()); ok {
			events = append(events, ev)
		}
	}
	return events
}

func TestParseFixtureNormal(t *testing.T) {
	events := parseFixture(t, "normal.jsonl")
	want := []adapter.Event{
		{Kind: adapter.EvSession, SessionID: "01a140b7-6c27-7441-b9a1-1a082df4d4c9"},
		{Kind: adapter.EvMessage, Text: "ok"},
		{Kind: adapter.EvUsage, Usage: &adapter.Usage{InputTokens: usageInt(11578), OutputTokens: usageInt(5), CachedInputTokens: usageInt(9984), ReasoningTokens: usageInt(0), ModelCalls: usageInt(1)}},
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %+v, want %+v (usage recorded, turn start ignored)", events, want)
	}
	if got := (&Adapter{}).ResultText(events); got != "ok" {
		t.Errorf("ResultText = %q, want ok", got)
	}
}

func TestParseFixtureTools(t *testing.T) {
	events := parseFixture(t, "tools.jsonl")
	const cmdText = `/bin/zsh -lc "sed -n '1p' note.md"`
	want := []adapter.Event{
		{Kind: adapter.EvSession, SessionID: "01a140b7-6c27-7441-b9a1-1a082df4d4c9"},
		{Kind: adapter.EvToolStart, Text: cmdText, Args: cmdText},
		{Kind: adapter.EvToolEnd, Text: cmdText, Args: cmdText},
		{Kind: adapter.EvMessage, Text: "the note starts with a fixture heading"},
		{Kind: adapter.EvUsage, Usage: &adapter.Usage{InputTokens: usageInt(11578), OutputTokens: usageInt(5), CachedInputTokens: usageInt(9984), ReasoningTokens: usageInt(0), ModelCalls: usageInt(1)}},
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %+v, want %+v", events, want)
	}
	if got := (&Adapter{}).ResultText(events); got != "the note starts with a fixture heading" {
		t.Errorf("ResultText = %q", got)
	}
}

func TestParseFixtureMCPTools(t *testing.T) {
	events := parseFixture(t, "mcp_tools.jsonl")
	want := []adapter.Event{
		{Kind: adapter.EvToolStart, Text: "brigada/plan_create", Args: "{}"},
		{Kind: adapter.EvToolEnd, Text: "brigada/plan_create"},
		// A failed call is the tool's outcome, not a fatal CLI error:
		// the end event names the failure and the run may still end clean.
		{Kind: adapter.EvToolEnd, Text: "brigada/status: bridge exploded"},
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %+v, want %+v", events, want)
	}
}

// TestParseFixtureMCPNullError pins the live misread of 2026-10-15: an
// item.completed mcp_tool_call with a non-completed status and
// "error":null read as a fatal error whose text was the four bytes
// "null". It is the tool's end with the status named, nothing more.
func TestParseFixtureMCPNullError(t *testing.T) {
	events := parseFixture(t, "mcp_null_error.jsonl")
	want := []adapter.Event{
		{Kind: adapter.EvSession, SessionID: "01a140f9-97d3-79c3-ae80-1b9bf2ad6c49"},
		{Kind: adapter.EvToolStart, Text: "brigada/task_restart", Args: "{}"},
		{Kind: adapter.EvToolEnd, Text: "brigada/task_restart: failed"},
		{Kind: adapter.EvMessage, Text: "the restart was refused; I planned around it"},
		{Kind: adapter.EvUsage, Usage: &adapter.Usage{InputTokens: usageInt(1), OutputTokens: usageInt(1), ModelCalls: usageInt(1)}},
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %+v, want %+v", events, want)
	}
}

func TestResultTextIsTheLastMessage(t *testing.T) {
	a := &Adapter{}
	events := []adapter.Event{
		{Kind: adapter.EvMessage, Text: "first"},
		{Kind: adapter.EvToolStart, Text: "ls"},
		{Kind: adapter.EvMessage, Text: "last"},
		{Kind: adapter.EvToolEnd, Text: "ls"},
	}
	if got := a.ResultText(events); got != "last" {
		t.Errorf("ResultText = %q, want last", got)
	}
	if got := a.ResultText(nil); got != "" {
		t.Errorf("ResultText(nil) = %q, want empty", got)
	}
}

func TestFailureFixtures(t *testing.T) {
	cases := []struct {
		file string
		kind adapter.ErrorKind
	}{
		{"failure-limit.jsonl", adapter.ErrLimit},
		{"failure-auth.jsonl", adapter.ErrAuth},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			var errs int
			for _, ev := range parseFixture(t, tc.file) {
				if ev.Kind != adapter.EvError {
					continue
				}
				errs++
				if ev.ErrorKind != tc.kind {
					t.Errorf("error kind = %q, want %q (%q)", ev.ErrorKind, tc.kind, ev.Text)
				}
			}
			if errs == 0 {
				t.Error("no error event")
			}
		})
	}
}

func TestParseEventIgnored(t *testing.T) {
	a := &Adapter{}
	for _, line := range []string{
		``,
		`   `,
		`not json at all`,
		`{"type":"turn.started"}`,
		`{"type":"turn.completed","usage":{"input_tokens":"bad"}}`,
		`{"type":"item.completed","item":{"id":"i","type":"reasoning","text":"hmm"}}`,
		`{"type":"item.completed","item":{"id":"i","type":"file_change"}}`,
		`{"type":"item.started","item":{"id":"i","type":"agent_message","text":"x"}}`,
		`{"type":"something.new"}`,
		`{"type":"item.completed"}`,
		`{"type":"thread.started"}`,
	} {
		if ev, ok := a.ParseEvent([]byte(line)); ok {
			t.Errorf("ParseEvent(%q) = %+v, want ignored", line, ev)
		}
	}
}

func TestToolTextIsBounded(t *testing.T) {
	long := strings.Repeat("x", 1000)
	line := `{"type":"item.started","item":{"id":"i","type":"command_execution","command":"echo hi\n` + long + `"}}`
	ev, ok := (&Adapter{}).ParseEvent([]byte(line))
	if !ok || ev.Kind != adapter.EvToolStart || ev.Text != "echo hi" {
		t.Errorf("event = %+v, want tool start with the first line", ev)
	}
	// The arguments keep the whole command (§5.6): two calls that share
	// a first line — heredocs above all — stay told apart, bounded only
	// by maxToolArgs.
	if want := "echo hi\n" + long; ev.Args != want {
		t.Errorf("args length = %d, want the whole command", len(ev.Args))
	}
	line = `{"type":"item.started","item":{"id":"i","type":"command_execution","command":"` + long + `"}}`
	ev, _ = (&Adapter{}).ParseEvent([]byte(line))
	if len([]rune(ev.Text)) != maxToolText {
		t.Errorf("text length = %d, want %d", len([]rune(ev.Text)), maxToolText)
	}
}

func TestErrorItemAndPlainFailure(t *testing.T) {
	a := &Adapter{}
	ev, ok := a.ParseEvent([]byte(`{"type":"item.completed","item":{"id":"i","type":"error","message":"rate limit exceeded"}}`))
	if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != adapter.ErrLimit {
		t.Errorf("error item = %+v, %v; want a limit error", ev, ok)
	}
	ev, ok = a.ParseEvent([]byte(`{"type":"error","message":"something odd"}`))
	if !ok || ev.ErrorKind != adapter.ErrOther {
		t.Errorf("unsorted error = %+v, %v; want kind other", ev, ok)
	}
}

func TestParseErrEvent(t *testing.T) {
	a := &Adapter{}
	cases := []struct {
		name string
		line string
		kind adapter.ErrorKind // "" = not an event
	}{
		{"usage limit", "ERROR: You've hit your usage limit. Try again later.", adapter.ErrLimit},
		{"not logged in", "Not logged in", adapter.ErrAuth},
		{"invalid key", "Invalid API key provided", adapter.ErrAuth},
		{"gone thread", "Error: no saved session found with ID abc", adapter.ErrResumeFailed},
		{"retry noise", "rate limited, retrying in 2s", ""},
		{"mcp noise", "mcp server foo: 401 unauthorized", ""},
		{"warning", "WARNING: something harmless", ""},
		{"blank", "", ""},
		{"json progress is stdout's", `{"type":"turn.started"}`, ""},
		{"json error", `{"type":"error","message":"You've hit your usage limit."}`, adapter.ErrLimit},
		{"json turn failed", `{"type":"turn.failed","error":{"message":"Please log in"}}`, adapter.ErrAuth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := a.ParseErrEvent([]byte(tc.line))
			if tc.kind == "" {
				if ok {
					t.Errorf("ParseErrEvent(%q) = %+v, want ignored", tc.line, ev)
				}
				return
			}
			if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != tc.kind {
				t.Errorf("ParseErrEvent(%q) = %+v, %v; want error kind %q", tc.line, ev, ok, tc.kind)
			}
		})
	}
}

// TestLimitReset covers the usage-limit reset note (§8): the dated,
// clock and span forms of "try again at/in …", as fixtures on both
// streams, at a fixed clock and time zone. The CLI prints its times in
// the machine's local zone, so the fixture zone is a fixed one.
func TestLimitReset(t *testing.T) {
	oldZone := localZone
	localZone = time.FixedZone("fixture", 2*60*60)
	t.Cleanup(func() { localZone = oldZone })
	oldNow := now
	now = func() time.Time { return time.Date(2026, 10, 20, 23, 30, 0, 0, localZone) }
	t.Cleanup(func() { now = oldNow })
	a := &Adapter{}
	cases := []struct {
		file  string
		until string
	}{
		// The live message of 2026-10-20, verbatim.
		{"limit-reset-verbatim.jsonl", "2026-10-24T23:42:00+02:00"},
		{"limit-reset-no-year.jsonl", "2026-10-24T23:42:00+02:00"},
		// A date already past this year rolls to the next one.
		{"limit-reset-no-year-past.jsonl", "2027-01-05T08:15:00+02:00"},
		{"limit-reset-clock.jsonl", "2026-10-20T23:42:00+02:00"},
		// A clock time already passed rolls to tomorrow.
		{"limit-reset-clock-tomorrow.jsonl", "2026-10-21T09:00:00+02:00"},
		{"limit-reset-in-minutes.jsonl", "2026-10-21T00:15:00+02:00"},
		{"limit-reset-in-hours.jsonl", "2026-10-21T01:30:00+02:00"},
		{"limit-reset-in-days.jsonl", "2026-10-23T23:30:00+02:00"},
		{"limit-reset-in-hm.jsonl", "2026-10-21T01:45:00+02:00"},
		// No time named leaves the reset unknown.
		{"failure-limit.jsonl", ""},
	}
	parsers := map[string]func([]byte) (adapter.Event, bool){"stdout": a.ParseEvent, "stderr": a.ParseErrEvent}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var limits int
			for name, parse := range parsers {
				sc := bufio.NewScanner(bytes.NewReader(data))
				for sc.Scan() {
					ev, ok := parse(sc.Bytes())
					if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != adapter.ErrLimit {
						continue
					}
					limits++
					if ev.LimitUntil != tc.until {
						t.Errorf("%s: LimitUntil = %q, want %q", name, ev.LimitUntil, tc.until)
					}
				}
			}
			if limits == 0 {
				t.Error("no limit error event in the fixture")
			}
		})
	}
}

// TestLimitResetUnparsable: a note whose time does not parse leaves
// LimitUntil empty, on both tables' paths, and never fails the event.
func TestLimitResetUnparsable(t *testing.T) {
	oldNow := now
	now = func() time.Time { return time.Date(2026, 10, 20, 23, 30, 0, 0, time.UTC) }
	t.Cleanup(func() { now = oldNow })
	a := &Adapter{}
	for _, tc := range []struct{ name, line string }{
		{"later", `{"type":"error","message":"You've hit your usage limit. Try again later."}`},
		{"bad month", `{"type":"error","message":"You've hit your usage limit. Try again at Xyz 24th, 2026 11:42 PM."}`},
		{"bad hour", `{"type":"error","message":"You've hit your usage limit. Try again at 13:42 PM."}`},
		{"empty span", `{"type":"error","message":"You've hit your usage limit. Try again in a while."}`},
		{"prose stderr", "ERROR: You've hit your usage limit. Try again whenever."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, parse := range map[string]func([]byte) (adapter.Event, bool){"stdout": a.ParseEvent, "stderr": a.ParseErrEvent} {
				ev, ok := parse([]byte(tc.line))
				if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != adapter.ErrLimit {
					t.Errorf("%s: event = %+v, ok = %v, want a limit error", name, ev, ok)
					continue
				}
				if ev.LimitUntil != "" {
					t.Errorf("%s: LimitUntil = %q, want empty", name, ev.LimitUntil)
				}
			}
		})
	}
}

// TestAttachCommandGolden pins the interactive argv. The opening prompt is
// positional in Codex's own TUI; unlike the headless command, no output,
// sandbox, or approval flags belong here.
func TestAttachCommandGolden(t *testing.T) {
	stubCLI(t)
	const prompt = "the foreman's opening prompt"
	dir := t.TempDir()
	cases := []struct {
		name   string
		ad     Adapter
		job    adapter.Job
		resume string
		want   []string
	}{
		{
			name: "fresh session",
			job:  adapter.Job{Prompt: prompt},
			want: []string{"codex", prompt},
		},
		{
			name:   "resume existing thread",
			ad:     Adapter{Binary: "/usr/local/bin/codex"},
			job:    adapter.Job{Prompt: prompt, Model: "gpt-5"},
			resume: "01a140b7-6c27-7441-b9a1-1a082df4d4c9",
			want:   []string{"/usr/local/bin/codex", "resume", "01a140b7-6c27-7441-b9a1-1a082df4d4c9", "-m", "gpt-5", prompt},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := tc.ad.AttachCommand(tc.job, dir, tc.resume, "client-session-id")
			if err != nil {
				t.Fatalf("AttachCommand: %v", err)
			}
			want := slices.Clone(tc.want)
			if tc.ad.Binary == "" {
				want[0] = "codex"
			}
			if filepath.Base(cmd.Path) != filepath.Base(want[0]) {
				t.Errorf("executable = %q, want %q", cmd.Path, want[0])
			}
			if got := cmd.Args[1:]; !slices.Equal(got, want[1:]) {
				t.Errorf("args = %q, want %q", got, want[1:])
			}
			if cmd.Dir != dir {
				t.Errorf("Dir = %q, want %q", cmd.Dir, dir)
			}
			if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
				t.Errorf("interactive command wires terminal streams: stdin %v stdout %v stderr %v", cmd.Stdin, cmd.Stdout, cmd.Stderr)
			}
			for _, arg := range cmd.Args {
				if arg == "--json" || arg == "-s" || arg == "--approve-for-me" {
					t.Errorf("headless/approval flag leaked into interactive argv: %q", arg)
				}
			}
		})
	}
}

func TestAttachReadingCheckoutIsReadOnly(t *testing.T) {
	stubCLI(t)
	cmd, err := (&Adapter{}).AttachCommand(adapter.Job{Prompt: "read code", MCPConfig: "/state/mcp/config.json"}, "/state/foreman/1/repo", "saved", "new")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"-s read-only", `approvals_reviewer="auto_review"`, "sandbox_approval=false", "request_permissions=false", "mcp_elicitations=true"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %s: %v", want, cmd.Args)
		}
	}
	for _, bad := range []string{"--approve-for-me", "--add-dir", "--dangerously-bypass-approvals-and-sandbox"} {
		if slices.Contains(cmd.Args, bad) {
			t.Errorf("unsafe flag %s: %v", bad, cmd.Args)
		}
	}
	if cmd.Dir != "/state/foreman/1/repo" {
		t.Errorf("session cwd changed: %s", cmd.Dir)
	}
}

// TestNoticeAndFinalMarks: the adapter is stateless, so it marks what
// the runner's per-job turn decides on — the error event and item carry
// Notice, turn.failed carries Final, stderr JSON errors carry neither —
// and StartsTurn spots turn.started.
func TestNoticeAndFinalMarks(t *testing.T) {
	a := &Adapter{}
	for _, line := range []string{
		`{"type":"error","message":"Reconnecting... 1/5"}`,
		`{"type":"item.completed","item":{"type":"error","message":"Model metadata not found"}}`,
	} {
		ev, ok := a.ParseEvent([]byte(line))
		if !ok || ev.Kind != adapter.EvError || !ev.Notice || ev.Final {
			t.Errorf("ParseEvent(%s) = %+v, %v; want a Notice error", line, ev, ok)
		}
	}
	ev, ok := a.ParseEvent([]byte(`{"type":"turn.failed","error":{"message":"boom"}}`))
	if !ok || ev.Kind != adapter.EvError || ev.Notice || !ev.Final || ev.Text != "boom" {
		t.Errorf("turn.failed = %+v, %v; want a Final error", ev, ok)
	}
	ev, ok = a.ParseErrEvent([]byte(`{"type":"error","message":"boom"}`))
	if !ok || ev.Notice {
		t.Errorf("stderr error = %+v, %v; want no Notice", ev, ok)
	}
	if !a.StartsTurn([]byte(`{"type":"turn.started"}`)) || a.StartsTurn([]byte(`{"type":"turn.failed"}`)) || a.StartsTurn([]byte(`junk`)) {
		t.Error("StartsTurn must be true for turn.started alone")
	}
}

// TestPreTurnFixtures: the streams parse to the marks the runner needs.
func TestPreTurnFixtures(t *testing.T) {
	failed := parseFixture(t, "preturn-error-failed.jsonl")
	var notices, finals int
	for _, ev := range failed {
		if ev.Kind == adapter.EvError && ev.Notice {
			notices++
		}
		if ev.Kind == adapter.EvError && ev.Final && ev.ErrorKind == adapter.ErrLimit && strings.Contains(ev.Text, "usage limit") {
			finals++
		}
	}
	if notices != 1 || finals != 1 {
		t.Errorf("failed stream: %d notices, %d finals; want 1 and 1 (%+v)", notices, finals, failed)
	}
	if !slices.ContainsFunc(parseFixture(t, "preturn-error-ok.jsonl"), func(e adapter.Event) bool { return e.Notice }) {
		t.Error("ok stream: no Notice error")
	}
}
