package fake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestParseEvent(t *testing.T) {
	a := &Adapter{}
	cases := []struct {
		name string
		line string
		want adapter.Event
		ok   bool
	}{
		{"session", `{"type":"system_init","session_id":"sess-7"}`,
			adapter.Event{Kind: adapter.EvSession, SessionID: "sess-7"}, true},
		{"session without id", `{"type":"system_init"}`, adapter.Event{}, false},
		{"message", `{"type":"assistant","text":"working on it"}`,
			adapter.Event{Kind: adapter.EvMessage, Text: "working on it"}, true},
		{"tool start by name", `{"type":"tool_start","name":"Edit"}`,
			adapter.Event{Kind: adapter.EvToolStart, Text: "Edit"}, true},
		{"tool end by text", `{"type":"tool_end","text":"Bash"}`,
			adapter.Event{Kind: adapter.EvToolEnd, Text: "Bash"}, true},
		{"result message", `{"type":"result","text":"{\"status\":\"done\"}"}`,
			adapter.Event{Kind: adapter.EvMessage, Text: `{"status":"done"}`}, true},
		{"limit error", `{"type":"error","kind":"limit","message":"quota exhausted"}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit, Text: "quota exhausted"}, true},
		{"auth error", `{"type":"error","kind":"auth","message":"expired token"}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrAuth, Text: "expired token"}, true},
		{"other error kind normalizes", `{"type":"error","kind":"weird"}`,
			adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: ""}, true},
		{"blank line", `   `, adapter.Event{}, false},
		{"unknown type", `{"type":"stream_header"}`, adapter.Event{}, false},
		// A line that is not an event — a banner, prose, broken JSON —
		// is not an error either: only the CLI's own error line is, so a
		// stray line on its stdout cannot fail an otherwise good run.
		{"not json", `oops`, adapter.Event{}, false},
		{"broken json", `{"type":"assistant","text":`, adapter.Event{}, false},
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

func TestResultText(t *testing.T) {
	a := &Adapter{}
	events := []adapter.Event{}
	for _, line := range []string{
		`{"type":"assistant","text":"first"}`,
		`{"type":"result","text":"the final message"}`,
	} {
		if ev, ok := a.ParseEvent([]byte(line)); ok {
			events = append(events, ev)
		}
	}
	if got := a.ResultText(events); got != "the final message" {
		t.Errorf("ResultText = %q, want the last message", got)
	}
	if got := a.ResultText(nil); got != "" {
		t.Errorf("ResultText(nil) = %q, want empty", got)
	}
}

// TestCommandRunsScript checks the plumbing end to end: the script runs
// in the workdir, reads the prompt on stdin and the resume id from the
// environment, and its stdout parses back through ParseEvent.
func TestCommandRunsScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "worker.sh")
	body := "#!/bin/sh\n" +
		"cat > prompt.txt\n" +
		"printf '%s\\n' \"$BRIGADA_FAKE_RESUME\" > resume.txt\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"text\":\"did it\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	a := &Adapter{Script: script}
	cmd, err := a.Command(adapter.Job{Prompt: "do the thing"}, dir, "sess-9")
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run script: %v\n%s", err, out)
	}
	if prompt, _ := os.ReadFile(filepath.Join(dir, "prompt.txt")); string(prompt) != "do the thing" {
		t.Errorf("prompt on stdin = %q, want the job's prompt", prompt)
	}
	if resume, _ := os.ReadFile(filepath.Join(dir, "resume.txt")); strings.TrimSpace(string(resume)) != "sess-9" {
		t.Errorf("resume id = %q, want sess-9 in the environment", resume)
	}
	ev, ok := a.ParseEvent(out)
	if !ok || ev.Kind != adapter.EvMessage || ev.Text != "did it" {
		t.Errorf("stdout event = %+v ok=%v, want the assistant message", ev, ok)
	}
	// A fresh run passes an empty resume id, not the adapter's absence.
	cmd, _ = a.Command(adapter.Job{Prompt: "fresh"}, dir, "")
	if cmd.Env == nil {
		t.Fatal("command has no environment")
	}
	var found bool
	for _, kv := range cmd.Env {
		if kv == "BRIGADA_FAKE_RESUME=" {
			found = true
		}
	}
	if !found {
		t.Errorf("fresh run env = %v, want BRIGADA_FAKE_RESUME set empty", cmd.Env)
	}
}
