package opencode

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Command tests inspect the invocation without running the real CLI;
// parser tests replay its recorded JSONL from testdata/.
func stubCLI(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\nexit 111\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestNameAndForemanCapability(t *testing.T) {
	a := &Adapter{}
	if a.Name() != "opencode" {
		t.Errorf("Name() = %q", a.Name())
	}
	if adapter.ResumeCapable(a.Name()) {
		t.Fatal("opencode cannot carry the foreman's MCP bridge")
	}
}

func TestCommand(t *testing.T) {
	stubCLI(t)
	const prompt = "--auto\nverbatim worker prompt with `quotes` and spaces"
	cases := []struct {
		name   string
		ad     Adapter
		model  string
		resume string
		want   []string
	}{
		{"fresh", Adapter{}, "", "", []string{"opencode", "run", "--format", "json", "--", prompt}},
		{"model", Adapter{}, "provider/model", "", []string{"opencode", "run", "--format", "json", "-m", "provider/model", "--", prompt}},
		{"resume with model", Adapter{}, "provider/model", "ses_123", []string{"opencode", "run", "--format", "json", "-m", "provider/model", "-s", "ses_123", "--", prompt}},
		{"resume CLI default", Adapter{}, DefaultModel, "ses_123", []string{"opencode", "run", "--format", "json", "-s", "ses_123", "--", prompt}},
		{"fresh CLI default", Adapter{}, DefaultModel, "", []string{"opencode", "run", "--format", "json", "--", prompt}},
		{"binary and preset", Adapter{Binary: "/cli/opencode", Agent: "worker"}, "", "", []string{"/cli/opencode", "run", "--format", "json", "--agent", "worker", "--", prompt}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cmd, err := tc.ad.Command(adapter.Job{Prompt: prompt, Model: tc.model}, dir, tc.resume)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cmd.Args, tc.want) {
				t.Errorf("args = %q, want %q", cmd.Args, tc.want)
			}
			if cmd.Dir != dir {
				t.Errorf("Dir = %q, want %q", cmd.Dir, dir)
			}
			for _, flag := range []string{"--auto", "--yolo", "--dangerously-skip-permissions", "-c", "--continue"} {
				if slices.Contains(cmd.Args, flag) {
					t.Errorf("unexpected flag %q", flag)
				}
			}
		})
	}
}

func TestCommandMCPConfigRefused(t *testing.T) {
	for _, resume := range []string{"", "ses_123"} {
		_, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", MCPConfig: "/bridge/config.json"}, t.TempDir(), resume)
		if err == nil || !strings.Contains(err.Error(), "MCP-config") {
			t.Errorf("resume %q: error = %v, want MCP-config refusal", resume, err)
		}
	}
}

func TestCommandEnv(t *testing.T) {
	stubCLI(t)
	t.Setenv("BRIGADA_RUNNER_TOKEN", "runner-private-value")
	t.Setenv("BRIGADA_ENGINE_SOCKET", "/private/brigada.sock")
	t.Setenv("OPENCODE_AMBIENT", "keep-me")
	cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "BRIGADA_") || strings.Contains(kv, "runner-private-value") {
			t.Errorf("runner secret reached the worker: %s", kv)
		}
	}
	if !slices.Contains(cmd.Env, "OPENCODE_AMBIENT=keep-me") {
		t.Error("CLI ambient environment was lost")
	}
}

func parseFixture(t *testing.T, name string, parse func([]byte) (adapter.Event, bool)) []adapter.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var events []adapter.Event
	for _, line := range strings.Split(string(data), "\n") {
		if ev, ok := parse([]byte(line)); ok {
			events = append(events, ev)
		}
	}
	return events
}

func TestNormalFixture(t *testing.T) {
	a := &Adapter{}
	events := parseFixture(t, "normal.jsonl", a.ParseEvent)
	want := []adapter.Event{
		{Kind: adapter.EvSession, SessionID: "ses_fixture"},
		{Kind: adapter.EvToolEnd, Text: "read"},
		{Kind: adapter.EvUsage, Usage: &adapter.Usage{InputTokens: usageInt(10), OutputTokens: usageInt(5), CachedInputTokens: usageInt(0), ReasoningTokens: usageInt(0)}},
		{Kind: adapter.EvSession, SessionID: "ses_fixture"},
		{Kind: adapter.EvMessage, Text: "The final answer text."},
		{Kind: adapter.EvUsage, Usage: &adapter.Usage{InputTokens: usageInt(10), OutputTokens: usageInt(5), CachedInputTokens: usageInt(0), ReasoningTokens: usageInt(0)}},
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %+v, want %+v", events, want)
	}
	if got := a.ResultText(events); got != "The final answer text." {
		t.Errorf("ResultText = %q", got)
	}
	var session string
	for _, ev := range events {
		if ev.Kind == adapter.EvSession {
			session = ev.SessionID
		}
	}
	if session != "ses_fixture" {
		t.Errorf("write-back session = %q", session)
	}
}

func TestFailureFixtures(t *testing.T) {
	a := &Adapter{}
	for _, tc := range []struct {
		file string
		kind adapter.ErrorKind
		text string
	}{
		{"failure-limit.jsonl", adapter.ErrLimit, "Usage limit reached: quota exhausted"},
		{"failure-auth.jsonl", adapter.ErrAuth, "Invalid API key"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			for _, parse := range []func([]byte) (adapter.Event, bool){a.ParseEvent, a.ParseErrEvent} {
				events := parseFixture(t, tc.file, parse)
				var failures []adapter.Event
				for _, ev := range events {
					if ev.Kind == adapter.EvError {
						failures = append(failures, ev)
					}
				}
				want := []adapter.Event{{Kind: adapter.EvError, ErrorKind: tc.kind, Text: tc.text, SessionID: "ses_fixture"}}
				if !slices.Equal(failures, want) {
					t.Errorf("failures = %+v, want %+v", failures, want)
				}
			}
		})
	}
}

func TestParseEvent(t *testing.T) {
	cases := []struct {
		name string
		line string
		want adapter.Event
		ok   bool
	}{
		// Running is a documented ToolState, but v1.19.0 does not emit
		// it on the JSON run path. This is a schema test, not a recording.
		{"running tool", `{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"running"}}}`, adapter.Event{Kind: adapter.EvToolStart, Text: "read"}, true},
		{"failed tool ends normally", `{"type":"tool_use","part":{"type":"tool","tool":"bash","state":{"status":"error","error":"401 is file content"}}}`, adapter.Event{Kind: adapter.EvToolEnd, Text: "bash"}, true},
		{"pending tool ignored", `{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"pending"}}}`, adapter.Event{}, false},
		{"unknown tool state", `{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"future"}}}`, adapter.Event{}, false},
		{"tool without name", `{"type":"tool_use","part":{"type":"tool","state":{"status":"completed"}}}`, adapter.Event{}, false},
		{"unknown event with session", `{"type":"future","sessionID":"ses_unknown","part":{"type":"text","text":"ignore"}}`, adapter.Event{}, false},
		{"server event is not CLI event", `{"type":"message.part.updated","properties":{"part":{"type":"text","text":"ignore"}}}`, adapter.Event{}, false},
		{"unknown part", `{"type":"text","part":{"type":"future","text":"ignore"}}`, adapter.Event{}, false},
		{"empty text", `{"type":"text","part":{"type":"text","text":""}}`, adapter.Event{}, false},
		{"missing session", `{"type":"step_start","part":{"type":"step-start"}}`, adapter.Event{}, false},
		{"missing error", `{"type":"error"}`, adapter.Event{}, false},
		{"unknown error shape", `{"type":"error","error":"quota"}`, adapter.Event{}, false},
		{"noise", "warning: quota 429", adapter.Event{}, false},
		{"blank", "  ", adapter.Event{}, false},
		{"broken JSON", `{"type":`, adapter.Event{}, false},
		{"non-object", `[]`, adapter.Event{}, false},
	}
	a := &Adapter{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := a.ParseEvent([]byte(tc.line))
			if ok != tc.ok || got != tc.want {
				t.Errorf("ParseEvent = (%+v, %v), want (%+v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		text   string
		want   adapter.ErrorKind
	}{
		{"APIError", 429, "request rejected", adapter.ErrLimit},
		{"APIError", 401, "request rejected", adapter.ErrAuth},
		{"APIError", 403, "request rejected", adapter.ErrAuth},
		{"ProviderAuthError", 0, "No credentials configured", adapter.ErrAuth},
		{"APIError", 0, "HTTP 429", adapter.ErrLimit},
		{"APIError", 0, "Usage-limit reached", adapter.ErrLimit},
		{"APIError", 0, "Rate limit exceeded", adapter.ErrLimit},
		{"APIError", 0, "Too many requests", adapter.ErrLimit},
		{"APIError", 0, "invalid_api_key", adapter.ErrAuth},
		{"APIError", 0, "Invalid-key", adapter.ErrAuth},
		{"APIError", 0, "Unauthorized (401)", adapter.ErrAuth},
		{"APIError", 0, "Forbidden (403)", adapter.ErrAuth},
		{"UnknownError", 0, "endpoint vanished", adapter.ErrOther},
	} {
		t.Run(tc.text, func(t *testing.T) {
			e := errorJSON{Name: tc.name}
			e.Data.StatusCode, e.Data.Message = tc.status, tc.text
			if got := errorKind(e); got != tc.want {
				t.Errorf("errorKind = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestResultText(t *testing.T) {
	a := &Adapter{}
	for _, tc := range []struct {
		name   string
		events []adapter.Event
		want   string
	}{
		{"no events", nil, ""},
		{"no messages", []adapter.Event{{Kind: adapter.EvToolEnd, Text: "read"}}, ""},
		{"last assistant wins", []adapter.Event{
			{Kind: adapter.EvMessage, Text: "narration"},
			{Kind: adapter.EvToolEnd, Text: "read"},
			{Kind: adapter.EvMessage, Text: "the answer"},
			{Kind: adapter.EvSession, SessionID: "ses_123"},
		}, "the answer"},
		{"later tool preserves last message", []adapter.Event{
			{Kind: adapter.EvMessage, Text: "last message"},
			{Kind: adapter.EvToolEnd, Text: "read"},
		}, "last message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.ResultText(tc.events); got != tc.want {
				t.Errorf("ResultText = %q, want %q", got, tc.want)
			}
		})
	}
}
