package codex

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

func TestNotAForemanHarness(t *testing.T) {
	if adapter.ResumeCapable("codex") {
		t.Error("codex is in ResumeCapableHarnesses; it cannot carry the foreman's MCP bridge")
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
			wantArg: append(slices.Clone(base), "--", prompt),
		},
		{
			name:    "fresh run with model",
			ad:      Adapter{Binary: "/usr/local/bin/codex"},
			job:     adapter.Job{Prompt: prompt, Model: "gpt-5"},
			wantExe: "/usr/local/bin/codex",
			wantArg: append(slices.Clone(base), "-m", "gpt-5", "--", prompt),
		},
		{
			name:    "resume names the thread",
			job:     adapter.Job{Prompt: prompt},
			resume:  "01a140b7-6c27-7441-b9a1-1a082df4d4c9",
			wantExe: "codex",
			wantArg: append(slices.Clone(base), "resume", "01a140b7-6c27-7441-b9a1-1a082df4d4c9", "--", prompt),
		},
		{
			name:    "the default model names none",
			job:     adapter.Job{Prompt: prompt, Model: DefaultModel},
			wantExe: "codex",
			wantArg: append(slices.Clone(base), "--", prompt),
		},
		{
			name:    "extra args pass through",
			ad:      Adapter{ExtraArgs: []string{"-c", "x=1"}},
			job:     adapter.Job{Prompt: prompt},
			wantExe: "codex",
			wantArg: append(slices.Clone(base), "-c", "x=1", "--", prompt),
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

func TestCommandMCPConfigRefused(t *testing.T) {
	stubCLI(t)
	if _, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", MCPConfig: "/tmp/mcp.json"}, t.TempDir(), ""); err == nil {
		t.Fatal("Command accepted an MCP config; the exec path has no flag for it")
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
	}
	if !slices.Equal(events, want) {
		t.Errorf("events = %+v, want %+v (usage and turn markers ignored)", events, want)
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
		{Kind: adapter.EvToolStart, Text: cmdText},
		{Kind: adapter.EvToolEnd, Text: cmdText},
		{Kind: adapter.EvMessage, Text: "the note starts with a fixture heading"},
	}
	if !slices.Equal(events, want) {
		t.Errorf("events = %+v, want %+v", events, want)
	}
	if got := (&Adapter{}).ResultText(events); got != "the note starts with a fixture heading" {
		t.Errorf("ResultText = %q", got)
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
		`{"type":"turn.completed","usage":{"input_tokens":1}}`,
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
