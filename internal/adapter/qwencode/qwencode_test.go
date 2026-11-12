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

func TestName(t *testing.T) {
	if got := (&Adapter{}).Name(); got != "qwen-code" {
		t.Errorf("Name() = %q", got)
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
	// --setting-sources user keeps the workspace's .qwen/settings.json out.
	base := []string{"-p", prompt, "-o", "stream-json", "--setting-sources", "user"}
	cases := []struct {
		name   string
		job    adapter.Job
		resume string
		want   []string
	}{
		{"fresh", adapter.Job{Prompt: prompt}, "", append(slicesClone(base), autoApproveFlag)},
		{"default model omits -m", adapter.Job{Prompt: prompt, Model: DefaultModel}, "", append(slicesClone(base), autoApproveFlag)},
		{"model", adapter.Job{Prompt: prompt, Model: "qwen-subscription/qwen3.8-flash"}, "", append(slicesClone(base), "-m", "qwen-subscription/qwen3.8-flash", autoApproveFlag)},
		{"resume", adapter.Job{Prompt: prompt}, "sess-1", append(slicesClone(base), autoApproveFlag, "--resume", "sess-1")},
		{"read dirs and tools ignored", adapter.Job{Prompt: prompt, ReadDirs: []string{"/x"}, Tools: []string{"Bash(go test:*)"}}, "", append(slicesClone(base), autoApproveFlag)},
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
		{"mcp", adapter.Job{Prompt: "p", MCPConfig: "/tmp/mcp.json"}, "MCP"},
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
	kinds := []adapter.EventKind{adapter.EvSession, adapter.EvToolStart, adapter.EvToolEnd, adapter.EvMessage, adapter.EvSession}
	if len(events) != len(kinds) {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	for i, k := range kinds {
		if events[i].Kind != k {
			t.Errorf("event %d kind = %q, want %q", i, events[i].Kind, k)
		}
	}
	const session = "7af062ac-b502-4f29-8990-cc063cf4060a"
	if events[0].SessionID != session {
		t.Errorf("init session = %q", events[0].SessionID)
	}
	if events[1].Text != "Write" || !strings.Contains(events[1].Args, "hello.txt") {
		t.Errorf("tool start = %+v", events[1])
	}
	if events[2].Text != "call_f9109cbe78d5447eb08b1e79" {
		t.Errorf("tool end = %+v", events[2])
	}
	if events[4].SessionID != session {
		t.Errorf("result session = %q", events[4].SessionID)
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
