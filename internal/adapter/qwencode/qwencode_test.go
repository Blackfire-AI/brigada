package qwencode

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// No test here runs qwen: Command is only built and inspected, and the
// stream is the fixtures in testdata/.

func stubCLI(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "qwen"), []byte("#!/bin/sh\nexit 111\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestNameAndForemanCapability(t *testing.T) {
	a := &Adapter{}
	if a.Name() != "qwen-code" {
		t.Errorf("Name() = %q", a.Name())
	}
	if !adapter.ResumeCapable(a.Name()) {
		t.Fatal("qwen-code must be a harness the foreman can run on: it resumes headless and carries the bridge")
	}
	var _ adapter.Interactive = a
	// The attach forks onto the client's minted id: no write-back.
	if _, ok := any(a).(adapter.SessionWriteback); ok {
		t.Error("qwen-code implements SessionWriteback; its attach names the minted id itself")
	}
}

func TestProbeCommand(t *testing.T) {
	stubCLI(t)
	cmd := (&Adapter{}).ProbeCommand(t.Context())
	if got := cmd.Args[1:]; !reflect.DeepEqual(got, []string{"--version"}) {
		t.Errorf("probe args = %v", got)
	}
}

func TestCommand(t *testing.T) {
	stubCLI(t)
	const prompt = "the task spec\nverbatim"
	dir := t.TempDir()
	base := []string{"-p", prompt, "-o", "stream-json"}
	// The flags every run carries, as `qwen --help` (1.0.0-preview.1) names
	// them: auto-approve, and the repository's own settings skipped.
	tail := []string{"--dangerously-skip-permissions", "--setting-sources", "user"}
	cases := []struct {
		name   string
		job    adapter.Job
		resume string
		want   []string
	}{
		{"fresh", adapter.Job{Prompt: prompt}, "", append(slicesClone(base), tail...)},
		{"default model omits -m", adapter.Job{Prompt: prompt, Model: DefaultModel}, "", append(slicesClone(base), tail...)},
		{"model", adapter.Job{Prompt: prompt, Model: "qwen-subscription/qwen3.8-flash"}, "", append(append(slicesClone(base), "-m", "qwen-subscription/qwen3.8-flash"), tail...)},
		{"resume", adapter.Job{Prompt: prompt}, "sess-1", append(append(slicesClone(base), tail...), "--resume", "sess-1")},
		{"read dirs and permission ignored", adapter.Job{Prompt: prompt, ReadDirs: []string{"/x"}, Permission: "plan"}, "", append(slicesClone(base), tail...)},
		// A foreman turn: the bridge, strict, then the resume.
		{"mcp", adapter.Job{Prompt: prompt, MCPConfig: "/tmp/mcp.json"}, "sess-1", append(append(slicesClone(base), tail...), "--mcp-config", "/tmp/mcp.json", "--strict-mcp-config", "--resume", "sess-1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := (&Adapter{}).Command(tc.job, dir, tc.resume)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(cmd.Path) != "qwen" {
				t.Errorf("exe = %q", cmd.Path)
			}
			if !reflect.DeepEqual(cmd.Args[1:], tc.want) {
				t.Errorf("args = %q, want %q", cmd.Args[1:], tc.want)
			}
			if cmd.Dir != dir {
				t.Errorf("Dir = %q", cmd.Dir)
			}
		})
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

func TestCommandEnvDropsBrigada(t *testing.T) {
	stubCLI(t)
	t.Setenv("BRIGADA_TOKEN", "secret")
	t.Setenv("QWEN_KEEP", "1")
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	var kept bool
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "BRIGADA_") {
			t.Errorf("env leaks %q", kv)
		}
		kept = kept || kv == "QWEN_KEEP=1"
	}
	if !kept {
		t.Error("ambient env dropped")
	}
}

func TestCommandRefusals(t *testing.T) {
	stubCLI(t)
	cases := []struct {
		name string
		job  adapter.Job
		want string
	}{
		{"review", adapter.Job{Prompt: "p", Review: true}, "no read-only review policy yet; review jobs cannot run on it"},
		{"effort", adapter.Job{Prompt: "p", Effort: "high"}, "effort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := (&Adapter{}).Command(tc.job, t.TempDir(), "")
			if err == nil || cmd != nil {
				t.Fatalf("Command = %v, %v; want refusal", cmd, err)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "qwen-code: ") {
				t.Errorf("err = %q", err)
			}
		})
	}
}

// TestCommandMCPConfig is the foreman turn's bridge as an argv assertion:
// a job carrying an MCP config is no longer refused, and the headless
// turn is strict.
func TestCommandMCPConfig(t *testing.T) {
	stubCLI(t)
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", MCPConfig: "/tmp/mcp.json"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "p", "-o", "stream-json", "--dangerously-skip-permissions", "--setting-sources", "user", "--mcp-config", "/tmp/mcp.json", "--strict-mcp-config"}
	if !reflect.DeepEqual(cmd.Args[1:], want) {
		t.Errorf("args = %q, want %q", cmd.Args[1:], want)
	}
}

func TestAttachCommand(t *testing.T) {
	stubCLI(t)
	const (
		prompt = "the opening prompt\nverbatim"
		config = "/state/mcp/turn-1/config.json"
		newID  = "11111111-2222-4333-8444-555555555555"
		status = `brigada status --line --state-dir "/x y"`
	)
	statusJSON := `{"statusLine":{"type":"command","command":"brigada status --line --state-dir \"/x y\""}}`
	tail := []string{"--setting-sources", "user", "--mcp-config", config}
	cases := []struct {
		name   string
		job    adapter.Job
		resume string
		want   []string
	}{
		{"fresh chat", adapter.Job{Prompt: prompt, MCPConfig: config}, "",
			append([]string{"-i", prompt, "--session-id", newID}, tail...)},
		{"resumed chat", adapter.Job{Prompt: prompt, MCPConfig: config}, "old-1",
			append([]string{"-i", prompt, "--resume", "old-1", "--fork-session", "--session-id", newID}, tail...)},
		{"fresh chat with status line", adapter.Job{Prompt: prompt, MCPConfig: config, StatusLine: status}, "",
			append(append([]string{"-i", prompt, "--session-id", newID}, tail...), "--settings", statusJSON)},
		{"resumed chat with status line and model", adapter.Job{Prompt: prompt, Model: "qwen-subscription/qwen3.8-max", MCPConfig: config, StatusLine: status}, "old-1",
			append(append([]string{"-i", prompt, "--resume", "old-1", "--fork-session", "--session-id", newID, "-m", "qwen-subscription/qwen3.8-max"}, tail...), "--settings", statusJSON)},
		{"default model and no prompt", adapter.Job{Model: DefaultModel, MCPConfig: config}, "",
			append([]string{"--session-id", newID}, tail...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cmd, err := (&Adapter{}).AttachCommand(tc.job, dir, tc.resume, newID)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(cmd.Path) != "qwen" {
				t.Errorf("exe = %q", cmd.Path)
			}
			if !reflect.DeepEqual(cmd.Args[1:], tc.want) {
				t.Errorf("args = %q, want %q", cmd.Args[1:], tc.want)
			}
			if cmd.Dir != dir {
				t.Errorf("Dir = %q", cmd.Dir)
			}
			if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
				t.Error("the attach wired a stream; they are the caller's")
			}
		})
	}
}

// TestStrictMCPConfigHeadlessOnly: the headless turn offers the bridge
// alone, while the attach loads the user's own servers beside it; neither
// the TUI nor the stream flags leak into the other.
func TestStrictMCPConfigHeadlessOnly(t *testing.T) {
	stubCLI(t)
	job := adapter.Job{Prompt: "p", MCPConfig: "/tmp/mcp.json"}
	headless, err := (&Adapter{}).Command(job, t.TempDir(), "s")
	if err != nil {
		t.Fatal(err)
	}
	attach, err := (&Adapter{}).AttachCommand(job, t.TempDir(), "s", "new")
	if err != nil {
		t.Fatal(err)
	}
	has := func(args []string, flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	if !has(headless.Args, "--strict-mcp-config") {
		t.Errorf("headless turn not strict: %q", headless.Args)
	}
	for _, flag := range []string{"--strict-mcp-config", "-p", "-o", "--dangerously-skip-permissions"} {
		if has(attach.Args, flag) {
			t.Errorf("attach carries %s: %q", flag, attach.Args)
		}
	}
}

func TestAttachCommandRefusals(t *testing.T) {
	stubCLI(t)
	cases := []struct {
		name  string
		job   adapter.Job
		newID string
		want  string
	}{
		{"no new id", adapter.Job{MCPConfig: "/c"}, "", "new session id"},
		{"no bridge", adapter.Job{}, "n", "no MCP config"},
		{"effort", adapter.Job{MCPConfig: "/c", Effort: "high"}, "n", "effort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := (&Adapter{}).AttachCommand(tc.job, t.TempDir(), "", tc.newID)
			if err == nil || cmd != nil {
				t.Fatalf("AttachCommand = %v, %v; want refusal", cmd, err)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "qwen-code: ") {
				t.Errorf("err = %q", err)
			}
		})
	}
}

func TestBinaryOverride(t *testing.T) {
	cmd, err := (&Adapter{Binary: "/bin/sh"}).Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != "/bin/sh" {
		t.Errorf("Path = %q", cmd.Path)
	}
}

func readLines(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		lines = append(lines, s.Text())
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestParseNormal(t *testing.T) {
	a := &Adapter{}
	var events []adapter.Event
	for _, l := range readLines(t, "normal.jsonl") {
		if ev, ok := a.ParseEvent([]byte(l)); ok {
			events = append(events, ev)
		}
	}
	kinds := []adapter.EventKind{adapter.EvSession, adapter.EvMessage, adapter.EvToolStart, adapter.EvToolEnd, adapter.EvToolStart, adapter.EvToolEnd, adapter.EvMessage, adapter.EvSession}
	if len(events) != len(kinds) {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	for i, k := range kinds {
		if events[i].Kind != k {
			t.Errorf("event %d kind = %q, want %q", i, events[i].Kind, k)
		}
	}
	const session = "4bd8be37-d417-483f-80fa-6a3b99eeaf5b"
	if events[0].SessionID != session {
		t.Errorf("init session = %q", events[0].SessionID)
	}
	if events[2].Text != "Write" || events[2].Args == "" {
		t.Errorf("tool start = %+v", events[2])
	}
	if events[3].Text != "call_92a561522cdc4830865427a8" {
		t.Errorf("tool end = %+v", events[3])
	}
	if events[7].SessionID != session {
		t.Errorf("result session = %q", events[7].SessionID)
	}
	if got := a.ResultText(events); got != "done" {
		t.Errorf("ResultText = %q", got)
	}
}

func TestResultTextFallback(t *testing.T) {
	a := &Adapter{}
	events := []adapter.Event{
		{Kind: adapter.EvMessage, Text: "first"},
		{Kind: adapter.EvToolStart, Text: "x"},
		{Kind: adapter.EvMessage, Text: "last"},
	}
	if got := a.ResultText(events); got != "last" {
		t.Errorf("ResultText = %q", got)
	}
	if got := a.ResultText(nil); got != "" {
		t.Errorf("ResultText(nil) = %q", got)
	}
}

func TestParseIgnoresNoise(t *testing.T) {
	a := &Adapter{}
	for _, l := range []string{"", "  ", "qwen 1.0.0-preview.1", `{"type":"system","subtype":"other","session_id":"s"}`, `{"type":"mystery"}`, `{"type":"assistant","message":{"content":[]}}`} {
		if ev, ok := a.ParseEvent([]byte(l)); ok {
			t.Errorf("ParseEvent(%q) = %+v, want none", l, ev)
		}
	}
}

func TestErrorResult(t *testing.T) {
	a := &Adapter{}
	cases := []struct {
		line string
		want adapter.ErrorKind
	}{
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Usage limit reached"}`, adapter.ErrLimit},
		{`{"type":"result","is_error":true,"result":"something odd happened"}`, adapter.ErrOther},
		{`{"type":"result","subtype":"error_max_turns","is_error":true}`, adapter.ErrOther},
	}
	for _, tc := range cases {
		for name, parse := range map[string]func([]byte) (adapter.Event, bool){"stdout": a.ParseEvent, "stderr": a.ParseErrEvent} {
			ev, ok := parse([]byte(tc.line))
			if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != tc.want {
				t.Errorf("%s %s = %+v, %v; want %q", name, tc.line, ev, ok, tc.want)
			}
		}
	}
}

func TestFailureFixtures(t *testing.T) {
	a := &Adapter{}
	cases := map[string]adapter.ErrorKind{
		"failure-auth.txt":   adapter.ErrAuth,
		"failure-limit.txt":  adapter.ErrLimit,
		"failure-resume.txt": adapter.ErrResumeFailed,
		"failure-infra.txt":  adapter.ErrInfra,
		"failure-model.txt":  adapter.ErrModel,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			var got adapter.ErrorKind
			for _, l := range readLines(t, name) {
				if ev, ok := a.ParseErrEvent([]byte(l)); ok && ev.Kind == adapter.EvError {
					got = ev.ErrorKind
				}
			}
			if got != want {
				t.Errorf("kind = %q, want %q", got, want)
			}
		})
	}
}

func TestStderrChatterIsNotAnEvent(t *testing.T) {
	a := &Adapter{}
	for _, l := range []string{"rate limited, retrying in 2s", "warning: deprecated flag", "MCP server requires authorization"} {
		if ev, ok := a.ParseErrEvent([]byte(l)); ok {
			t.Errorf("ParseErrEvent(%q) = %+v, want none", l, ev)
		}
	}
}
