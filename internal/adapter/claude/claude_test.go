package claude

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/mcp"
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
				"--model", "sonnet", "--permission-mode", "acceptEdits",
				"--allowedTools", "Bash(go build:*)", "Bash(go test:*)", "Bash(go vet:*)", "Bash(gofmt:*)"},
		},
		{
			name:    "resume run, default binary, no model",
			job:     adapter.Job{Prompt: prompt},
			resume:  "sess-9",
			wantExe: "claude",
			wantArg: []string{"-p", "--output-format", "stream-json", "--verbose",
				"--resume", "sess-9", "--permission-mode", "acceptEdits",
				"--allowedTools", "Bash(go build:*)", "Bash(go test:*)", "Bash(go vet:*)", "Bash(gofmt:*)"},
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

// foremanTokenValue stands in for the lease-scoped foreman token
// (§12.2) — distinctive enough that an argument or an environment entry
// carrying it cannot pass for anything else.
const foremanTokenValue = "foreman-token-4b7e2d1a9c"

// TestCommandForemanScope is the foreman turn's golden argv (§4, §12.2):
// a job that carries an MCP config gets the bridge — --mcp-config with
// --strict-mcp-config, so no other server rides along — and brigada's
// tool scope in place of the agent config's: the bridge's own tools
// (the one tool table's whole offer, in the CLI's mcp__ spelling) plus
// the read-only file tools, with the shell and every editing tool
// denied by name. The foreman never edits code.
//
// The token itself — set in the environment the way the bridge's would
// be — reaches neither the command line nor the worker's environment:
// argv is public to anything listing processes, and the token is the
// bridge's alone.
func TestCommandForemanScope(t *testing.T) {
	stubCLI(t)
	t.Setenv(mcp.EnvToken, foremanTokenValue)
	const prompt = "the foreman's wake prompt"
	const config = "/runner-state/mcp/turn-x1/config.json"
	wantScope := []string{"--allowedTools", "Read", "Grep", "Glob",
		"mcp__brigada__decision_note",
		"mcp__brigada__plan_create", "mcp__brigada__plan_amend", "mcp__brigada__answer", "mcp__brigada__task_restart",
		"mcp__brigada__status", "mcp__brigada__history", "mcp__brigada__events",
		"mcp__brigada__capacity",
		"--disallowedTools", "Bash", "Edit", "NotebookEdit", "Write"}
	cases := []struct {
		name    string
		ad      Adapter
		wantArg []string
	}{
		{
			name: "bare adapter",
			ad:   Adapter{Binary: "/usr/local/bin/claude"},
			wantArg: append([]string{"-p", "--output-format", "stream-json", "--verbose",
				"--permission-mode", "acceptEdits",
				"--mcp-config", config, "--strict-mcp-config"}, wantScope...),
		},
		{
			name: "the foreman scope replaces the agent config's",
			ad: Adapter{
				PermissionMode:  "bypassPermissions",
				AllowedTools:    []string{"Bash(git:*)", "Edit"},
				DisallowedTools: []string{"WebFetch"},
			},
			wantArg: append([]string{"-p", "--output-format", "stream-json", "--verbose",
				"--permission-mode", "bypassPermissions",
				"--mcp-config", config, "--strict-mcp-config"}, wantScope...),
		},
	}
	for _, tc := range cases {
		cmd, err := tc.ad.Command(adapter.Job{Prompt: prompt, MCPConfig: config}, t.TempDir(), "")
		if err != nil {
			t.Fatalf("%s: build command: %v", tc.name, err)
		}
		want := append([]string{"/usr/local/bin/claude"}, tc.wantArg...)
		if tc.ad.Binary == "" {
			want[0] = "claude"
		}
		if !slices.Equal(cmd.Args, want) {
			t.Errorf("%s: args =\n%v\nwant\n%v", tc.name, cmd.Args, want)
		}
		for _, arg := range cmd.Args {
			if strings.Contains(arg, foremanTokenValue) {
				t.Errorf("%s: argv carries the foreman token: %v", tc.name, cmd.Args)
			}
		}
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, mcp.EnvToken+"=") || strings.Contains(kv, foremanTokenValue) {
				t.Errorf("%s: the worker's environment carries the foreman token: %s", tc.name, kv)
			}
		}
	}
}

// TestAttachCommandGolden is the attached session's golden argv (§4.2
// item 1): the CLI interactive — no -p, nothing to parse — with the
// opening prompt as its initial prompt, the bridge and the foreman's
// tool scope exactly as a headless turn carries them, and the session
// flags that make the write-back possible: a first session starts
// under the client's --session-id alone, a resume forks the key's
// session onto it with --fork-session. The prompt leads the argv —
// the tool-scope flags are variadic, and a positional after them would
// parse as one more tool name, leaving the session with no prompt. The
// streams stay unwired — the terminal is the caller's — and the hold
// token, like every brigada secret, reaches neither argv nor the
// environment.
func TestAttachCommandGolden(t *testing.T) {
	stubCLI(t)
	t.Setenv(mcp.EnvToken, foremanTokenValue)
	const prompt = "the foreman's opening prompt"
	const config = "/runner-state/mcp/turn-x1/config.json"
	const newID = "9f1d2b34-51a6-4c8e-a2d7-0b3c4d5e6f70"
	wantScope := []string{"--allowedTools", "Read", "Grep", "Glob",
		"mcp__brigada__decision_note",
		"mcp__brigada__plan_create", "mcp__brigada__plan_amend", "mcp__brigada__answer", "mcp__brigada__task_restart",
		"mcp__brigada__status", "mcp__brigada__history", "mcp__brigada__events",
		"mcp__brigada__capacity",
		"--disallowedTools", "Bash", "Edit", "NotebookEdit", "Write"}
	cases := []struct {
		name    string
		ad      Adapter
		job     adapter.Job
		resume  string
		wantArg []string
	}{
		{
			name: "first session, with the model",
			ad:   Adapter{Binary: "/usr/local/bin/claude"},
			job:  adapter.Job{Prompt: prompt, Model: "sonnet", MCPConfig: config},
			wantArg: append([]string{prompt, "--session-id", newID, "--model", "sonnet",
				"--mcp-config", config, "--strict-mcp-config"}, wantScope...),
		},
		{
			name:   "resume forks onto the new id",
			ad:     Adapter{Binary: "/usr/local/bin/claude"},
			job:    adapter.Job{Prompt: prompt, Model: "sonnet", MCPConfig: config},
			resume: "sess-first",
			wantArg: append([]string{prompt, "--resume", "sess-first", "--fork-session",
				"--session-id", newID, "--model", "sonnet",
				"--mcp-config", config, "--strict-mcp-config"}, wantScope...),
		},
		{
			name: "a status line rides as this session's settings",
			ad:   Adapter{Binary: "/usr/local/bin/claude"},
			job:  adapter.Job{Prompt: prompt, MCPConfig: config, StatusLine: "'/opt/brigada' status --line"},
			wantArg: append([]string{prompt, "--session-id", newID,
				"--mcp-config", config, "--strict-mcp-config",
				"--settings", `{"statusLine":{"command":"'/opt/brigada' status --line","refreshInterval":3,"type":"command"}}`}, wantScope...),
		},
		{
			name: "no model adds no flag",
			ad:   Adapter{},
			job:  adapter.Job{Prompt: prompt, MCPConfig: config},
			wantArg: append([]string{prompt, "--session-id", newID,
				"--mcp-config", config, "--strict-mcp-config"}, wantScope...),
		},
	}
	for _, tc := range cases {
		cmd, err := tc.ad.AttachCommand(tc.job, t.TempDir(), tc.resume, newID)
		if err != nil {
			t.Fatalf("%s: build the attached session: %v", tc.name, err)
		}
		want := append([]string{"/usr/local/bin/claude"}, tc.wantArg...)
		if tc.ad.Binary == "" {
			want[0] = "claude"
		}
		if !slices.Equal(cmd.Args, want) {
			t.Errorf("%s: args =\n%v\nwant\n%v", tc.name, cmd.Args, want)
		}
		// The terminal is the caller's: the adapter wires no stream,
		// unlike the headless command whose stdin carries the prompt.
		if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
			t.Errorf("%s: the attached session has streams wired: stdin %v, stdout %v, stderr %v",
				tc.name, cmd.Stdin, cmd.Stdout, cmd.Stderr)
		}
		for _, arg := range cmd.Args {
			if strings.Contains(arg, foremanTokenValue) {
				t.Errorf("%s: argv carries the foreman token: %v", tc.name, cmd.Args)
			}
		}
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, mcp.EnvToken+"=") || strings.Contains(kv, foremanTokenValue) {
				t.Errorf("%s: the attached environment carries the foreman token: %s", tc.name, kv)
			}
		}
	}
}

// TestAttachCommandRefusesWhatItCannotBuild covers the refusals (§4.2
// item 1): an attach without a generated session id has nothing to
// name to the CLI, and one without the bridge's config is a chat
// without brigada's tools — neither is built half-way.
func TestAttachCommandRefusesWhatItCannotBuild(t *testing.T) {
	stubCLI(t)
	a := &Adapter{}
	if _, err := a.AttachCommand(adapter.Job{Prompt: "p", MCPConfig: "/tmp/config.json"}, "", "", ""); err == nil {
		t.Error("an attach without a new session id was built")
	}
	if _, err := a.AttachCommand(adapter.Job{Prompt: "p"}, "", "", "sess-new"); err == nil {
		t.Error("an attach without an MCP config was built")
	}
}

// TestCommandWorkerPathUnchanged covers the other side of §4: a worker
// job — no MCP config — is built exactly as it always was, with no MCP
// flag and the agent config's own tool scope.
func TestCommandWorkerPathUnchanged(t *testing.T) {
	stubCLI(t)
	ad := Adapter{AllowedTools: []string{"Bash(git:*)"}}
	cmd, err := ad.Command(adapter.Job{Prompt: "p", Model: "sonnet"}, t.TempDir(), "sess-9")
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	want := []string{"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--model", "sonnet", "--resume", "sess-9", "--permission-mode", "acceptEdits",
		"--allowedTools", "Bash(git:*)"}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("worker args =\n%v\nwant\n%v", cmd.Args, want)
	}
}

// TestCommandWorkerDefaultScope pins the code worker's default tool scope
// (§5.2): a worker with no explicit scope may run the Go toolchain's
// build, test, vet and format commands and nothing else of the shell, and
// denies nothing by name. A foreman turn — a job with an MCP config — is
// not a worker: it gets the foreman's scope, as TestCommandForemanScope
// pins it.
func TestCommandWorkerDefaultScope(t *testing.T) {
	stubCLI(t)
	var ad Adapter
	cmd, err := ad.Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("build worker command: %v", err)
	}
	want := []string{"--allowedTools",
		"Bash(go build:*)", "Bash(go test:*)", "Bash(go vet:*)", "Bash(gofmt:*)"}
	if !slices.Equal(cmd.Args[len(cmd.Args)-len(want):], want) {
		t.Errorf("worker args = %v\nwant them to end with %v", cmd.Args, want)
	}
	if slices.Contains(cmd.Args, "--disallowedTools") {
		t.Errorf("worker args deny tools by default: %v", cmd.Args)
	}

	// An explicit scope replaces the default; an explicit none is file
	// tools only — no shell flag at all, not the default.
	over := Adapter{AllowedTools: []string{"Bash(npm test:*)", "Bash(npx jest:*)"}}
	cmd, err = over.Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("build override command: %v", err)
	}
	wantOver := []string{"--allowedTools", "Bash(npm test:*)", "Bash(npx jest:*)"}
	if !slices.Equal(cmd.Args[len(cmd.Args)-len(wantOver):], wantOver) || slices.Contains(cmd.Args, "Bash(go test:*)") {
		t.Errorf("override args = %v, want them to end with %v and drop the Go default", cmd.Args, wantOver)
	}
	none := Adapter{AllowedTools: []string{}}
	cmd, err = none.Command(adapter.Job{Prompt: "p"}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("build none command: %v", err)
	}
	if slices.Contains(cmd.Args, "--allowedTools") || slices.Contains(cmd.Args, "Bash(go test:*)") {
		t.Errorf("tools=none args = %v, want no allowed tools at all", cmd.Args)
	}

	const config = "/runner-state/mcp/turn-x1/config.json"
	// A worker scope never reaches a foreman turn.
	foremanAd := Adapter{AllowedTools: []string{"Bash"}}
	foremanBypass, err := foremanAd.Command(adapter.Job{Prompt: "p", MCPConfig: config}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("build foreman command: %v", err)
	}
	if slices.Contains(foremanBypass.Args, "Bash") && !slices.Contains(foremanBypass.Args[slices.Index(foremanBypass.Args, "--disallowedTools"):], "Bash") {
		t.Errorf("a worker scope reached the foreman: %v", foremanBypass.Args)
	}
	foreman, err := ad.Command(adapter.Job{Prompt: "p", MCPConfig: config}, t.TempDir(), "")
	if err != nil {
		t.Fatalf("build foreman command: %v", err)
	}
	wantForeman := []string{"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "acceptEdits", "--mcp-config", config, "--strict-mcp-config",
		"--allowedTools", "Read", "Grep", "Glob",
		"mcp__brigada__decision_note",
		"mcp__brigada__plan_create", "mcp__brigada__plan_amend", "mcp__brigada__answer", "mcp__brigada__task_restart",
		"mcp__brigada__status", "mcp__brigada__history", "mcp__brigada__events",
		"mcp__brigada__capacity",
		"--disallowedTools", "Bash", "Edit", "NotebookEdit", "Write"}
	if !slices.Equal(foreman.Args, wantForeman) {
		t.Errorf("foreman args =\n%v\nwant\n%v", foreman.Args, wantForeman)
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

// TestSessionLimit covers the CLI's session-limit message: a limit on
// both streams, with the reset time when it parses and empty when not.
func TestSessionLimit(t *testing.T) {
	defer func(old func() time.Time) { now = old }(now)
	now = func() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) }
	a := &Adapter{}
	live := "You've hit your session limit · resets 6:40pm (Europe/Moscow)"
	cases := []struct {
		name, line, until string
	}{
		{"live message", live, "2026-10-15T18:40:00+03:00"},
		{"live message as error result", `{"type":"result","is_error":true,"result":"` + live + `"}`, "2026-10-15T18:40:00+03:00"},
		{"time already past rolls to tomorrow", "You've hit your session limit · resets 9:00am (Europe/Moscow)", "2026-10-16T09:00:00+03:00"},
		{"no time suffix", "You've hit your session limit", ""},
		{"unknown zone", "You've hit your session limit · resets 6:40pm (Mars/Olympus)", ""},
		{"bad hour", "You've hit your session limit · resets 13:40pm (Europe/Moscow)", ""},
	}
	parsers := map[string]func([]byte) (adapter.Event, bool){"stdout": a.ParseEvent, "stderr": a.ParseErrEvent}
	for _, tc := range cases {
		for name, parse := range parsers {
			ev, ok := parse([]byte(tc.line))
			if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != adapter.ErrLimit {
				t.Errorf("%s/%s: event = %+v, ok = %v, want a limit error", tc.name, name, ev, ok)
				continue
			}
			if ev.LimitUntil != tc.until {
				t.Errorf("%s/%s: LimitUntil = %q, want %q", tc.name, name, ev.LimitUntil, tc.until)
			}
		}
	}
}

func TestForemanReadingCheckout(t *testing.T) {
	stubCLI(t)
	ad := &Adapter{PermissionMode: "bypassPermissions"}
	job := adapter.Job{MCPConfig: "/runner/mcp/config.json", ReadOnlyRepo: "/runner/foreman/1/repo"}
	headless, err := ad.Command(job, "/runner/foreman/1", "saved")
	if err != nil {
		t.Fatal(err)
	}
	attached, err := ad.AttachCommand(job, "/runner/foreman/1", "saved", "new")
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []*exec.Cmd{headless, attached} {
		i := slices.Index(cmd.Args, "--add-dir")
		if i < 0 || i+1 >= len(cmd.Args) || cmd.Args[i+1] != job.ReadOnlyRepo {
			t.Errorf("missing reading scope: %v", cmd.Args)
		}
		i = slices.Index(cmd.Args, "--disallowedTools")
		if i < 0 {
			t.Fatalf("missing edit denials: %v", cmd.Args)
		}
		for _, denied := range []string{"Bash", "Edit", "NotebookEdit", "Write"} {
			if !slices.Contains(cmd.Args[i+1:], denied) {
				t.Errorf("%s no longer denied: %v", denied, cmd.Args)
			}
		}
		if !slices.Contains(cmd.Env, "CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=1") {
			t.Error("additional CLAUDE.md loading disabled")
		}
		if cmd.Dir != "/runner/foreman/1" {
			t.Errorf("session cwd changed: %s", cmd.Dir)
		}
	}
}

// TestScopeText pins the registration's spelling of a worker scope.
func TestScopeText(t *testing.T) {
	if got, want := ScopeText(nil), "Bash(go build:*)+Bash(go test:*)+Bash(go vet:*)+Bash(gofmt:*)"; got != want {
		t.Errorf("default scope text = %q, want %q", got, want)
	}
	if got := ScopeText([]string{}); got != "none" {
		t.Errorf("explicit none text = %q, want none", got)
	}
	if got := ScopeText([]string{"Bash(npm test:*)", "Bash(npx jest:*)"}); got != "Bash(npm test:*)+Bash(npx jest:*)" {
		t.Errorf("override text = %q", got)
	}
}
