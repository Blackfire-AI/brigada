package dsh

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Command tests inspect the invocation without running the real CLI;
// parser tests replay the spike's lines from testdata/. The patch files
// Command writes go to a per-test temp directory.
func stubCLI(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dsh"), []byte("#!/bin/sh\nexit 111\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TMPDIR", t.TempDir())
}

func envValue(env []string, key string) (string, bool) {
	var v string
	var ok bool
	for _, kv := range env {
		if rest, found := strings.CutPrefix(kv, key+"="); found {
			v, ok = rest, true
		}
	}
	return v, ok
}

func patchArg(t *testing.T, args []string) string {
	t.Helper()
	i := slices.Index(args, "--patch")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no --patch in %v", args)
	}
	return args[i+1]
}

func TestNameAndForemanCapability(t *testing.T) {
	a := &Adapter{}
	if a.Name() != "dsh" {
		t.Errorf("Name() = %q", a.Name())
	}
	if adapter.ResumeCapable(a.Name()) {
		t.Fatal("dsh is worker-only: no MCP-config flag is known")
	}
}

func TestCommand(t *testing.T) {
	stubCLI(t)
	a := &Adapter{}
	cmd, err := a.Command(adapter.Job{Prompt: "do the thing", Model: "deepseek-v4-pro", Effort: "high"}, "/work", "")
	if err != nil {
		t.Fatal(err)
	}
	patch := patchArg(t, cmd.Args)
	// Launcher options must precede app options: dsh stops parsing its own at the first app flag.
	want := []string{"dsh", "--profile", "headless", "--patch", patch, "--json", "-"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("args = %v, want %v", cmd.Args, want)
	}
	if cmd.Dir != "/work" {
		t.Errorf("Dir = %q", cmd.Dir)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "do the thing") {
		t.Error("the prompt rides stdin, not the arguments")
	}
	if stdin, err := io.ReadAll(cmd.Stdin); err != nil || string(stdin) != "do the thing" {
		t.Errorf("stdin = %q, %v", stdin, err)
	}

	cmd, err = a.Command(adapter.Job{Prompt: "p", Model: DefaultModel}, "/work", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	patch = patchArg(t, cmd.Args)
	want = []string{"dsh", "--profile", "headless", "--patch", patch, "--json", "--session-id", "session-1", "-"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("resume args = %v, want %v", cmd.Args, want)
	}
}

func TestPatchFile(t *testing.T) {
	stubCLI(t)
	worktree := t.TempDir()
	for _, tc := range []struct{ model, effort, want string }{
		{"deepseek-v4-pro", "max", "- id: agent-default-model\n  config:\n    provider: deepseek-official\n    model: \"deepseek-v4-pro\"\n    reasoningEffort: \"max\"\n"},
		{"deepseek-flash", "", "- id: agent-default-model\n  config:\n    provider: deepseek-official\n    model: \"deepseek-flash\"\n"},
		{"", "off", "- id: agent-default-model\n  config:\n    provider: deepseek-official\n    model: \"deepseek-flash\"\n    reasoningEffort: \"off\"\n"},
		{"odd\nmodel: x", "", "- id: agent-default-model\n  config:\n    provider: deepseek-official\n    model: \"odd\\nmodel: x\"\n"},
	} {
		cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Model: tc.model, Effort: tc.effort}, worktree, "")
		if err != nil {
			t.Fatal(err)
		}
		path := patchArg(t, cmd.Args)
		if strings.HasPrefix(path, worktree) {
			t.Errorf("patch %s is inside the worktree", path)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("patch for %q/%q =\n%s\nwant\n%s", tc.model, tc.effort, got, tc.want)
		}
	}
}

func TestCommandRefusals(t *testing.T) {
	stubCLI(t)
	a := &Adapter{}
	if _, err := a.Command(adapter.Job{Prompt: "p", MCPConfig: "/x/mcp.json"}, "/work", ""); err == nil {
		t.Error("an MCP config was accepted")
	}
	for _, effort := range []string{"medium", "xhigh", "none"} {
		if _, err := a.Command(adapter.Job{Prompt: "p", Effort: effort}, "/work", ""); err == nil || !strings.Contains(err.Error(), "dsh") {
			t.Errorf("effort %q: err = %v", effort, err)
		}
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := a.Command(adapter.Job{Prompt: "p"}, "/work", ""); err == nil {
		t.Error("a missing binary was accepted")
	}
}

func TestCommandEnv(t *testing.T) {
	stubCLI(t)
	t.Setenv("DEEPSEEK_API_KEY", "key-from-the-runner-env")
	t.Setenv("BRIGADA_TOKEN", "secret")
	t.Setenv("DSH_TELEMETRY_MODE", "ENABLED")
	t.Setenv("DSH_PERMISSION_MODE", "danger-full-access")
	t.Setenv("GOCACHE", "/ambient/go-build")
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	for _, tc := range []struct {
		review bool
		mode   string
	}{{false, "workspace-write"}, {true, "read-only"}} {
		cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Review: tc.review}, "/work", "")
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := envValue(cmd.Env, "DSH_TELEMETRY_MODE"); v != "DISABLED" {
			t.Errorf("review=%v: DSH_TELEMETRY_MODE = %q", tc.review, v)
		}
		if v, _ := envValue(cmd.Env, "DSH_PERMISSION_MODE"); v != tc.mode {
			t.Errorf("review=%v: DSH_PERMISSION_MODE = %q, want %q", tc.review, v, tc.mode)
		}
		if n := countKey(cmd.Env, "DSH_PERMISSION_MODE") + countKey(cmd.Env, "DSH_TELEMETRY_MODE"); n != 2 {
			t.Errorf("review=%v: %d DSH_ variables, want one each", tc.review, n)
		}
		if v, ok := envValue(cmd.Env, "DEEPSEEK_API_KEY"); !ok || v != "key-from-the-runner-env" {
			t.Errorf("review=%v: DEEPSEEK_API_KEY not passed", tc.review)
		}
		if _, ok := envValue(cmd.Env, "BRIGADA_TOKEN"); ok {
			t.Errorf("review=%v: BRIGADA_TOKEN leaked", tc.review)
		}
		if slices.Contains(cmd.Env, "DSH_PERMISSION_MODE=danger-full-access") {
			t.Errorf("review=%v: danger-full-access survived", tc.review)
		}
		cache, _ := envValue(cmd.Env, "GOCACHE")
		locks, _ := envValue(cmd.Env, "GIT_OPTIONAL_LOCKS")
		if tc.review {
			if cache != "/ambient/go-build" || locks != "1" {
				t.Errorf("review: GOCACHE=%q GIT_OPTIONAL_LOCKS=%q, want the ambient values", cache, locks)
			}
			continue
		}
		if want := filepath.Join(os.TempDir(), "brigada-gocache"); cache != want {
			t.Errorf("work: GOCACHE = %q, want %q", cache, want)
		}
		if locks != "0" {
			t.Errorf("work: GIT_OPTIONAL_LOCKS = %q, want 0", locks)
		}
		if n := countKey(cmd.Env, "GOCACHE") + countKey(cmd.Env, "GIT_OPTIONAL_LOCKS"); n != 2 {
			t.Errorf("work: %d GOCACHE/GIT_OPTIONAL_LOCKS entries, want one each", n)
		}
	}
}

func countKey(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			n++
		}
	}
	return n
}

func parseFixture(t *testing.T, name string, parse func([]byte) (adapter.Event, bool)) []adapter.Event {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []adapter.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if ev, ok := parse(sc.Bytes()); ok {
			events = append(events, ev)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func n64(n int64) *int64 { return &n }

func TestWorkRunFixture(t *testing.T) {
	a := &Adapter{}
	events := parseFixture(t, "work-run.jsonl", a.ParseEvent)
	want := []adapter.Event{
		{Kind: adapter.EvSession, SessionID: "session-249eec6f-212d-4f7d-87ca-1b3721617152"},
		{Kind: adapter.EvMessage, Text: "I'll start by checking the current state of the working directory and hello.txt."},
		{Kind: adapter.EvToolStart, Text: "bash", Args: `{"command":"pwd && ls -la && echo \"---hello.txt---\" && cat hello.txt","description":"Inspect directory and hello.txt"}`},
		{Kind: adapter.EvToolEnd, Text: "call_00_ET_5UZCrHdfqMiFkqnUalcZ0841"},
		{Kind: adapter.EvUsage, Usage: &adapter.Usage{InputTokens: n64(5787), OutputTokens: n64(90), CachedInputTokens: n64(0)}},
		{Kind: adapter.EvToolStart, Text: "bash", Args: `{"command":"xxd hello.txt","description":"Check trailing newline in hello.txt"}`},
		{Kind: adapter.EvUsage, Usage: &adapter.Usage{InputTokens: n64(166 + 6272), OutputTokens: n64(58), CachedInputTokens: n64(6272)}},
		{Kind: adapter.EvMessage, Text: "Done. Appended world to hello.txt."},
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %+v, want %+v", events, want)
	}
	if got := a.ResultText(events); got != "Done. Appended world to hello.txt." {
		t.Errorf("ResultText = %q", got)
	}
	var session string
	var sum adapter.Usage
	for _, ev := range events {
		switch ev.Kind {
		case adapter.EvSession:
			session = ev.SessionID
		case adapter.EvUsage:
			sum.Add(ev.Usage)
			if ev.IsProgress() {
				t.Error("usage advances progress")
			}
		}
	}
	if session != "session-249eec6f-212d-4f7d-87ca-1b3721617152" {
		t.Errorf("session = %q", session)
	}
	wantSum := adapter.Usage{InputTokens: n64(5787 + 166 + 6272), OutputTokens: n64(148), CachedInputTokens: n64(6272)}
	if !reflect.DeepEqual(sum, wantSum) {
		t.Errorf("usage sum = %+v, want %+v", sum, wantSum)
	}
}

func TestResultTextFallsBackToLastText(t *testing.T) {
	a := &Adapter{}
	events := []adapter.Event{
		{Kind: adapter.EvMessage, Text: "first"},
		{Kind: adapter.EvToolStart, Text: "bash"},
		{Kind: adapter.EvMessage, Text: "last words"},
		{Kind: adapter.EvToolEnd},
		{Kind: adapter.EvUsage},
	}
	if got := a.ResultText(events); got != "last words" {
		t.Errorf("ResultText = %q", got)
	}
	if got := a.ResultText(nil); got != "" {
		t.Errorf("ResultText(nil) = %q", got)
	}
}

func TestFailureFixtures(t *testing.T) {
	a := &Adapter{}
	for _, tc := range []struct {
		file string
		kind adapter.ErrorKind
		text string
	}{
		{"failure-missing-credential.jsonl", adapter.ErrAuth, "llm-deepseek: no API key for provider route \"deepseek-official\"; store DEEPSEEK_API_KEY through the credentials service (the web Models page writes it), or export DEEPSEEK_API_KEY in the launching environment"},
		{"failure-auth.jsonl", adapter.ErrAuth, "Authentication Fails, Your api key: ****-key is invalid (request_id: 00000000-0000-0000-0000-000000000000)"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			for _, parse := range []func([]byte) (adapter.Event, bool){a.ParseEvent, a.ParseErrEvent} {
				got := parseFixture(t, tc.file, parse)
				want := []adapter.Event{{Kind: adapter.EvError, ErrorKind: tc.kind, Text: tc.text}}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("events = %+v, want %+v", got, want)
				}
			}
		})
	}
}

func TestStderrFixture(t *testing.T) {
	a := &Adapter{}
	got := parseFixture(t, "failure-auth.stderr.txt", a.ParseErrEvent)
	want := []adapter.Event{{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth, Text: "Authentication Fails, Your api key: ****-key is invalid (request_id: 00000000-0000-0000-0000-000000000000)"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events = %+v, want %+v", got, want)
	}
	// Stdout's parser does not read the stderr line.
	if got := parseFixture(t, "failure-auth.stderr.txt", a.ParseEvent); len(got) != 0 {
		t.Errorf("ParseEvent read a stderr line: %+v", got)
	}
}

func TestParseErrEventIgnoresNoise(t *testing.T) {
	a := &Adapter{}
	for _, line := range []string{
		"",
		"dsh: warning: telemetry is disabled",
		"dsh: running with the headless profile",
		"Error: quota exceeded",
		`{"type":"text","text":"hello"}`,
		`{"type":"status","phase":"turn_end","turn":1,"reason":{"kind":"completed"}}`,
		"not json",
	} {
		if ev, ok := a.ParseErrEvent([]byte(line)); ok {
			t.Errorf("%q parsed as %+v", line, ev)
		}
	}
}

func TestErrorKinds(t *testing.T) {
	a := &Adapter{}
	for _, tc := range []struct {
		name, line string
		kind       adapter.ErrorKind
	}{
		{"auth code", `{"type":"status","phase":"turn_end","reason":{"kind":"error","error":{"message":"x","code":"AUTH"}}}`, adapter.ErrAuth},
		{"missing credential", `{"type":"status","phase":"turn_end","reason":{"kind":"error","error":{"message":"x","code":"MISSING_CREDENTIAL"}}}`, adapter.ErrAuth},
		{"status 401", `{"type":"status","phase":"turn_end","reason":{"kind":"error","error":{"message":"x","code":"HTTP","status":401}}}`, adapter.ErrAuth},
		{"status 429", `{"type":"status","phase":"turn_end","reason":{"kind":"error","error":{"message":"x","code":"HTTP","status":429}}}`, adapter.ErrLimit},
		{"status 402", `{"type":"status","phase":"turn_end","reason":{"kind":"error","error":{"message":"x","code":"HTTP","status":402}}}`, adapter.ErrLimit},
		{"rate limit text", `{"type":"status","phase":"turn_end","reason":{"kind":"error","error":{"message":"Rate limit reached for requests","code":"X"}}}`, adapter.ErrLimit},
		{"balance text", `{"type":"status","phase":"turn_end","reason":{"kind":"error","error":{"message":"Insufficient Balance","code":"X"}}}`, adapter.ErrLimit},
		{"other", `{"type":"status","phase":"turn_end","reason":{"kind":"error","error":{"message":"upstream exploded","code":"INTERNAL","status":500}}}`, adapter.ErrOther},
		{"no detail", `{"type":"status","phase":"turn_end","reason":{"kind":"error"}}`, adapter.ErrOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := a.ParseEvent([]byte(tc.line))
			if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != tc.kind || ev.Text == "" {
				t.Errorf("got %+v ok=%v, want an error of kind %s", ev, ok, tc.kind)
			}
		})
	}
	for _, tc := range []struct {
		line string
		kind adapter.ErrorKind
	}{
		{"dsh: MISSING_CREDENTIAL: no API key", adapter.ErrAuth},
		{"dsh: RATE_LIMITED: Rate limit exceeded, retry later", adapter.ErrLimit},
		{"dsh: API: Insufficient Balance", adapter.ErrLimit},
		{"dsh: INTERNAL: boom", adapter.ErrOther},
	} {
		ev, ok := a.ParseErrEvent([]byte(tc.line))
		if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != tc.kind {
			t.Errorf("%q: got %+v ok=%v, want kind %s", tc.line, ev, ok, tc.kind)
		}
	}
}

func TestParseEvent(t *testing.T) {
	a := &Adapter{}
	for _, line := range []string{
		"",
		"not json",
		`{"type":"thinking","text":"hmm"}`,
		`{"type":"status","phase":"turn_start","turn":1}`,
		`{"type":"status","phase":"step_start","turn":1,"step":1}`,
		`{"type":"status","phase":"turn_end","turn":1,"reason":{"kind":"completed"}}`,
		`{"type":"status","phase":"step_end","turn":1,"step":1}`,
		`{"type":"session"}`,
		`{"type":"text","text":""}`,
		`{"type":"banner"}`,
	} {
		if ev, ok := a.ParseEvent([]byte(line)); ok {
			t.Errorf("%q parsed as %+v", line, ev)
		}
	}
	ev, ok := a.ParseEvent([]byte(`{"type":"tool_result","callId":"c1","status":"error","result":"denied"}`))
	if !ok || ev.Kind != adapter.EvToolEnd {
		t.Errorf("an errored tool result is a finished call: %+v ok=%v", ev, ok)
	}
}
