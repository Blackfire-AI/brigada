package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	if !adapter.ResumeCapable(a.Name()) {
		t.Fatal("opencode must be a harness the foreman can run on: it resumes headless and carries the bridge")
	}
	var _ adapter.Interactive = a
	var _ adapter.SessionWriteback = a
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

// writeTurnConfig mimics the runner's per-session MCP config (§12.2): a
// private directory holding the harness-neutral config the runner writes,
// returned as the path a job's MCPConfig carries.
func writeTurnConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	config := `{"mcpServers":{"brigada":{"command":"/usr/local/bin/brigada","args":["mcp","--socket","/run/brigada/api.sock"],"env":{"BRIGADA_TOKEN":"lease-secret-9"}}}}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandForeman(t *testing.T) {
	stubCLI(t)
	t.Setenv("BRIGADA_RUNNER_TOKEN", "runner-private-value")
	config := writeTurnConfig(t)
	const foremanScope = `OPENCODE_PERMISSION={"bash":"deny","edit":"deny","external_directory":"deny","webfetch":"deny"}`
	for _, resume := range []string{"", "ses_123"} {
		cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Model: "provider/model", MCPConfig: config}, t.TempDir(), resume)
		if err != nil {
			t.Fatalf("resume %q: %v", resume, err)
		}
		if (resume != "") != slices.Contains(cmd.Args, "-s") {
			t.Errorf("resume %q: args = %q", resume, cmd.Args)
		}
		wantDir := filepath.Dir(config)
		if !slices.Contains(cmd.Env, "OPENCODE_CONFIG_DIR="+wantDir) {
			t.Errorf("resume %q: the bridge's config directory is not named to the CLI among %q", resume, cmd.Env)
		}
		if !slices.Contains(cmd.Env, foremanScope) {
			t.Errorf("resume %q: the foreman scope is not the review's own keys among %q", resume, cmd.Env)
		}
		if !slices.Contains(cmd.Env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
			t.Errorf("resume %q: the repository's opencode config is not disabled", resume)
		}
		for _, kv := range cmd.Env {
			if strings.Contains(kv, "runner-private-value") || strings.Contains(kv, "lease-secret-9") {
				t.Errorf("resume %q: a secret reached the CLI's environment: %s", resume, kv)
			}
		}
		// The bridge's own config, written beside the runner's, carries
		// the token where only the CLI and the bridge read it.
		bridge, err := os.ReadFile(filepath.Join(wantDir, "opencode.json"))
		if err != nil {
			t.Fatalf("resume %q: %v", resume, err)
		}
		var got struct {
			MCP map[string]struct {
				Type        string            `json:"type"`
				Command     []string          `json:"command"`
				Environment map[string]string `json:"environment"`
			} `json:"mcp"`
		}
		if err := json.Unmarshal(bridge, &got); err != nil {
			t.Fatalf("resume %q: parse the bridge's config: %v", resume, err)
		}
		server, ok := got.MCP["brigada"]
		if !ok || server.Type != "local" || !slices.Equal(server.Command, []string{"/usr/local/bin/brigada", "mcp", "--socket", "/run/brigada/api.sock"}) || !reflect.DeepEqual(server.Environment, map[string]string{"BRIGADA_TOKEN": "lease-secret-9"}) {
			t.Errorf("resume %q: bridge server = %+v", resume, server)
		}
	}
	// A config the runner's own write would answer with is refused
	// plainly, never run half-configured.
	if _, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", MCPConfig: filepath.Join(t.TempDir(), "gone.json")}, t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "gone.json") {
		t.Errorf("a missing MCP config: error = %v, want it named", err)
	}
}

func TestAttachCommand(t *testing.T) {
	stubCLI(t)
	t.Setenv("BRIGADA_RUNNER_TOKEN", "runner-private-value")
	t.Setenv("PWD", "/the/users/own/directory")
	if _, err := (&Adapter{}).AttachCommand(adapter.Job{Prompt: "p"}, t.TempDir(), "ses_1", "minted-1"); err == nil || !strings.Contains(err.Error(), "MCP config") {
		t.Errorf("a job without the bridge: error = %v, want the missing MCP config named", err)
	}
	config := writeTurnConfig(t)
	const prompt = "opening prompt with `quotes` and\nnewlines"
	cmd, err := (&Adapter{}).AttachCommand(adapter.Job{Prompt: prompt, Model: "provider/model", Effort: "high", MCPConfig: config}, "/checkout", "ses_9", "minted-9")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"opencode", "-s", "ses_9", "-m", "provider/model", "--variant", "high", "--prompt", prompt}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %q, want %q", cmd.Args, want)
	}
	if cmd.Dir != "/checkout" {
		t.Errorf("Dir = %q, want /checkout", cmd.Dir)
	}
	for _, flag := range []string{"--auto", "--yolo", "--dangerously-skip-permissions", "--fork"} {
		if slices.Contains(cmd.Args, flag) {
			t.Errorf("unexpected flag %q", flag)
		}
	}
	if !slices.Contains(cmd.Env, "PWD=/checkout") {
		t.Errorf("the checkout is not named as PWD among %q", cmd.Env)
	}
	if slices.ContainsFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "OLDPWD=") || kv == "PWD=/the/users/own/directory" }) {
		t.Errorf("an inherited PWD survived among %q", cmd.Env)
	}
	if !slices.Contains(cmd.Env, "OPENCODE_CONFIG_DIR="+filepath.Dir(config)) {
		t.Errorf("the bridge's config directory is not named to the CLI among %q", cmd.Env)
	}
	if !slices.Contains(cmd.Env, `OPENCODE_PERMISSION={"bash":"deny","edit":"deny","external_directory":"deny","webfetch":"deny"}`) {
		t.Errorf("the foreman scope is missing among %q", cmd.Env)
	}
	for _, kv := range cmd.Env {
		if strings.Contains(kv, "runner-private-value") {
			t.Errorf("a runner secret reached the chat: %s", kv)
		}
	}
	// A fresh chat carries no session flag at all.
	fresh, err := (&Adapter{}).AttachCommand(adapter.Job{Prompt: "p", MCPConfig: config}, "/checkout", "", "minted-9")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(fresh.Args, "-s") {
		t.Errorf("a fresh chat names a session: %q", fresh.Args)
	}
}

// stubSessionListCLI writes a stub opencode whose `session list --format
// json` prints the given JSON; any other invocation fails.
func stubSessionListCLI(t *testing.T, list string) string {
	t.Helper()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(fixture, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "opencode")
	// /bin/cat, not cat: an earlier stubCLI in the same test may have
	// narrowed PATH to a directory that holds nothing but the CLI.
	script := "#!/bin/sh\ncase \"$*\" in *\"session list --format json\"*) /bin/cat '" + fixture + "' ;; *) exit 111 ;; esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWritebackSessionID(t *testing.T) {
	// A chat that resumed a session keeps its id: the CLI is not asked.
	stubCLI(t)
	id, err := (&Adapter{}).WritebackSessionID(context.Background(), t.TempDir(), "ses_kept")
	if err != nil || id != "ses_kept" {
		t.Errorf("writeback = %q, %v; want ses_kept, no error", id, err)
	}

	// A fresh chat's id is discovered from the CLI's own list: the
	// newest session of the checkout wins, other directories and older
	// ones of the same checkout do not, and a checkout stored resolved
	// (/private/... on macOS) still matches.
	checkout := t.TempDir()
	resolved, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	list := fmt.Sprintf(`[
		{"id":"ses_other","directory":"/somewhere/else","updated":99},
		{"id":"ses_old","directory":%q,"updated":10},
		{"id":"ses_new","directory":%q,"updated":42}
	]`, resolved, checkout)
	a := &Adapter{Binary: stubSessionListCLI(t, list)}
	id, err = a.WritebackSessionID(context.Background(), checkout, "")
	if err != nil || id != "ses_new" {
		t.Errorf("writeback = %q, %v; want ses_new, no error", id, err)
	}

	// No session of the checkout: an error, never a guess.
	a = &Adapter{Binary: stubSessionListCLI(t, `[]`)}
	if _, err := a.WritebackSessionID(context.Background(), checkout, ""); err == nil {
		t.Error("an empty session list must be an error")
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
	if !slices.Contains(cmd.Env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
		t.Error("the repository's opencode config is not disabled")
	}
	if !slices.Contains(cmd.Env, "OPENCODE_AMBIENT=keep-me") {
		t.Error("CLI ambient environment was lost")
	}
}

func TestWorkerPermissions(t *testing.T) {
	// The Go default scope maps to these bash rules ("*" sorts first).
	const goBash = `{"*":"deny","go build":"allow","go build *":"allow","go test":"allow","go test *":"allow","go vet":"allow","go vet *":"allow","gofmt":"allow","gofmt *":"allow"}`
	const deny = `OPENCODE_PERMISSION={"bash":` + goBash + `,"external_directory":"deny"}`
	const schema = "/runner-state/worktrees/task-9-author-s0.extra/schema"
	const live = "/runner-state/worktrees/task-9-author-s0.extra/live"
	// The per-pattern form sorts "*" first (json.Marshal sorts a map's
	// keys), so the per-dir allows after it win as the last match.
	const allowed = `OPENCODE_PERMISSION={"bash":` + goBash + `,"external_directory":{"*":"deny","` + live + `":"allow","` + live + `/**":"allow","` + schema + `":"allow","` + schema + `/**":"allow"}}`
	cases := []struct {
		name     string
		ambient  string // set when non-empty
		readDirs []string
		want     string
	}{
		{"absent", "", nil, deny},
		{"other rules kept, bash replaced", `{"bash":"ask","edit":"allow"}`, nil, `OPENCODE_PERMISSION={"bash":` + goBash + `,"edit":"allow","external_directory":"deny"}`},
		{"weaker rule overridden", `{"external_directory":"allow"}`, nil, deny},
		{"invalid replaced", "not json", nil, deny},
		{"read dirs allowed, everything else denied", "", []string{schema, live}, allowed},
		{"read dirs replace a weaker object", `{"external_directory":{"*":"allow"}}`, []string{schema},
			`OPENCODE_PERMISSION={"bash":` + goBash + `,"external_directory":{"*":"deny","` + schema + `":"allow","` + schema + `/**":"allow"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubCLI(t)
			if tc.ambient != "" {
				t.Setenv("OPENCODE_PERMISSION", tc.ambient)
			}
			cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", ReadDirs: tc.readDirs}, t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			var got string
			for _, kv := range cmd.Env {
				if strings.HasPrefix(kv, "OPENCODE_PERMISSION=") {
					got = kv
				}
			}
			if got != tc.want {
				t.Errorf("env = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCommandCarriesJobTools(t *testing.T) {
	cases := []struct {
		name   string
		tools  []string
		review bool
		want   string
	}{
		{"project scope", []string{"Bash(npm test:*)"}, false, `"bash":{"*":"deny","npm test":"allow","npm test *":"allow"}`},
		{"none", []string{}, false, `"bash":"deny"`},
		{"review", []string{"Bash(npm test:*)"}, true, `"bash":"deny","edit":"deny"`},
		{"review fetch", nil, true, `"webfetch":"deny"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubCLI(t)
			cmd, err := (&Adapter{}).Command(adapter.Job{Prompt: "p", Tools: tc.tools, Review: tc.review}, t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			var got string
			for _, kv := range cmd.Env {
				if strings.HasPrefix(kv, "OPENCODE_PERMISSION=") {
					got = kv
				}
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("env = %q, want it to hold %s", got, tc.want)
			}
		})
	}
}

func TestBashRules(t *testing.T) {
	cases := []struct {
		name   string
		tools  []string
		review bool
		want   string
	}{
		{"default scope", nil, false, `{"*":"deny","go build":"allow","go build *":"allow","go test":"allow","go test *":"allow","go vet":"allow","go vet *":"allow","gofmt":"allow","gofmt *":"allow"}`},
		{"project scope", []string{"Bash(npm test:*)", "Bash(git log:*)"}, false, `{"*":"deny","git log":"allow","git log *":"allow","npm test":"allow","npm test *":"allow"}`},
		{"unmappable left out", []string{"Read", "Bash(*)", "Bash(rm -rf *:*)", "Bash(go test)", "Bash(a;b:*)", "Bash(:*)", "Bash(go test:*)"}, false, `{"*":"deny","go test":"allow","go test *":"allow"}`},
		{"nothing maps", []string{"Read", "WebFetch"}, false, `"deny"`},
		{"explicit none", []string{}, false, `"deny"`},
		{"review", []string{"Bash(go test:*)"}, true, `"deny"`},
		{"review default", nil, true, `"deny"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(bashRules(tc.tools, tc.review))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("bashRules = %s, want %s", got, tc.want)
			}
		})
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
		{Kind: adapter.EvUsage, Usage: &adapter.Usage{InputTokens: usageInt(10), OutputTokens: usageInt(5), CachedInputTokens: usageInt(0), ReasoningTokens: usageInt(0), ModelCalls: usageInt(1)}},
		{Kind: adapter.EvSession, SessionID: "ses_fixture"},
		{Kind: adapter.EvMessage, Text: "The final answer text."},
		{Kind: adapter.EvUsage, Usage: &adapter.Usage{InputTokens: usageInt(10), OutputTokens: usageInt(5), CachedInputTokens: usageInt(0), ReasoningTokens: usageInt(0), ModelCalls: usageInt(1)}},
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

// fixResetClock pins the clock and the zone limitReset reads.
func fixResetClock(t *testing.T) {
	t.Helper()
	oldNow, oldZone := now, localZone
	t.Cleanup(func() { now, localZone = oldNow, oldZone })
	now = func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) }
	localZone = time.FixedZone("test", 2*3600)
}

func TestLimitResetFixture(t *testing.T) {
	fixResetClock(t)
	a := &Adapter{}
	const text = "Weekly/Monthly Limit Exhausted. Your limit will reset at 2026-10-20 21:32:04"
	for name, parse := range map[string]func([]byte) (adapter.Event, bool){"stdout": a.ParseEvent, "stderr": a.ParseErrEvent} {
		events := parseFixture(t, "failure-limit-reset.jsonl", parse)
		want := []adapter.Event{{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit, Text: text, SessionID: "ses_fixture", LimitUntil: "2026-10-20T21:32:04+02:00"}}
		if !slices.Equal(events, want) {
			t.Errorf("%s: events = %+v, want %+v", name, events, want)
		}
	}
}

// TestLimitResetZoneFixture replays limit messages through adapters
// whose Command named the job's model: a Z.ai model's zone-less reset is
// read in Beijing wall-clock (UTC+8) whatever the machine's zone (the
// test zone is +02:00), a reset further away than the window the message
// names is clamped to now + window, and a non-Z.ai model keeps today's
// machine-zone read.
func TestLimitResetZoneFixture(t *testing.T) {
	fixResetClock(t)
	stubCLI(t)
	for _, tc := range []struct {
		name, model, file, text, want string
	}{
		{"zai zone-less is UTC+8", "zai-coding-plan/glm-4.6", "failure-limit-reset-zai.jsonl",
			"Weekly/Monthly Limit Exhausted. Your limit will reset at 2026-10-20 21:32:04",
			"2026-10-20T21:32:04+08:00"},
		{"zai reset beyond the window clamped", "zai-coding-plan/glm-4.6", "failure-limit-reset-zai-window.jsonl",
			"Weekly/Monthly Limit Exhausted. Your limit will reset at 2026-10-21 05:32:04 for 5 hour",
			"2026-10-20T14:00:00Z"},
		{"zhipuai zone-less is UTC+8", "zhipuai/glm-5", "failure-limit-reset-zai.jsonl",
			"Weekly/Monthly Limit Exhausted. Your limit will reset at 2026-10-20 21:32:04",
			"2026-10-20T21:32:04+08:00"},
		{"non-Z.ai model unchanged", "anthropic/claude-opus-4.6", "failure-limit-reset.jsonl",
			"Weekly/Monthly Limit Exhausted. Your limit will reset at 2026-10-20 21:32:04",
			"2026-10-20T21:32:04+02:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Adapter{}
			if _, err := a.Command(adapter.Job{Prompt: "p", Model: tc.model}, t.TempDir(), ""); err != nil {
				t.Fatal(err)
			}
			events := parseFixture(t, tc.file, a.ParseEvent)
			want := []adapter.Event{{Kind: adapter.EvError, ErrorKind: adapter.ErrLimit, Text: tc.text, SessionID: "ses_fixture", LimitUntil: tc.want}}
			if !slices.Equal(events, want) {
				t.Errorf("events = %+v, want %+v", events, want)
			}
		})
	}
}

func TestLimitReset(t *testing.T) {
	fixResetClock(t)
	const zai = "zai-coding-plan/glm-4.6"
	for _, tc := range []struct{ text, model, want string }{
		{"Your limit will reset at 2026-10-20 21:32:04", "", "2026-10-20T21:32:04+02:00"},
		{"Your limit will reset at 2026-10-20 21:32", "", "2026-10-20T21:32:00+02:00"},
		{"Your limit will reset at 2026-10-20T21:32:04Z.", "", "2026-10-20T21:32:04Z"},
		{"Your limit will reset at 2026-10-20 21:32:04 UTC", "", "2026-10-20T21:32:04Z"},
		{"Your limit will reset at 2026-10-20 21:32:04+08:00", "", "2026-10-20T21:32:04+08:00"},
		{"Your limit will reset at 2026-10-20 21:32:04 -0500", "", "2026-10-20T21:32:04-05:00"},
		{"Your limit will reset at 2026-10-20 07:00:00", "", ""}, // already past
		{"Your limit will reset at 2026-13-20 21:32:04", "", ""},
		{"Your limit will reset at 2026-02-31 21:32:04", "", ""},
		{"Your limit will reset soon", "", ""},
		{"", "", ""},
		// A Z.ai model's zone-less reset is Beijing wall-clock (UTC+8);
		// an explicit zone keeps its own, and other models are unchanged.
		{"Your limit will reset at 2026-10-20 21:32:04", zai, "2026-10-20T21:32:04+08:00"},
		{"Your limit will reset at 2026-10-20 21:32:04+08:00", zai, "2026-10-20T21:32:04+08:00"},
		{"Your limit will reset at 2026-10-20T21:32:04Z", zai, "2026-10-20T21:32:04Z"},
		{"Your limit will reset at 2026-10-20 21:32:04", "zhipuai/glm-5", "2026-10-20T21:32:04+08:00"},
		{"Your limit will reset at 2026-10-20 21:32:04", "anthropic/claude-opus-4.6", "2026-10-20T21:32:04+02:00"},
		// A reset further away than the window the message names is
		// clamped to now + window.
		{"Your limit will reset at 2026-10-21 05:32:04 for 5 hour", zai, "2026-10-20T14:00:00Z"},
		{"Your limit will reset at 2026-10-20 20:30:00 for 5 hour", zai, "2026-10-20T20:30:00+08:00"}, // inside the window: kept
		{"Your limit will reset at 2026-10-20 21:32:04 for 5 hour", "", "2026-10-20T14:00:00Z"},
	} {
		if got := limitReset(tc.text, tc.model); got != tc.want {
			t.Errorf("limitReset(%q, %q) = %q, want %q", tc.text, tc.model, got, tc.want)
		}
	}
}

func TestPermissionRejectFixture(t *testing.T) {
	a := &Adapter{}
	const text = "permission requested: external_directory (/Users/worker/projects/.worktrees/brigada-build-bare/internal/engine/*); auto-rejecting"
	events := parseFixture(t, "permission-reject.txt", a.ParseErrEvent)
	want := []adapter.Event{
		{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: text},
		{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: text},
	}
	if !slices.Equal(events, want) {
		t.Errorf("events = %+v, want %+v", events, want)
	}
	// The notice rides stderr only; the JSON stream must not read it.
	data, err := os.ReadFile(filepath.Join("testdata", "permission-reject.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		if _, ok := a.ParseEvent([]byte(line)); ok {
			t.Errorf("ParseEvent read a stderr notice: %q", line)
		}
	}
}

func TestParseErrEvent(t *testing.T) {
	const plain = "! permission requested: external_directory (/tmp/other/*); auto-rejecting"
	cases := []struct {
		name string
		line string
		want adapter.Event
		ok   bool
	}{
		{"coloured", "\x1b[93m\x1b[1m! \x1b[0mpermission requested: external_directory (/tmp/other/*); auto-rejecting", adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: "permission requested: external_directory (/tmp/other/*); auto-rejecting"}, true},
		{"plain", plain, adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: "permission requested: external_directory (/tmp/other/*); auto-rejecting"}, true},
		{"several patterns", "! permission requested: bash (git push, rm -rf *); auto-rejecting", adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: "permission requested: bash (git push, rm -rf *); auto-rejecting"}, true},
		{"other permission", "! permission requested: webfetch (https://example.invalid/*); auto-rejecting", adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: "permission requested: webfetch (https://example.invalid/*); auto-rejecting"}, true},
		{"no marker", "permission requested: external_directory (/tmp/*); auto-rejecting", adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: "permission requested: external_directory (/tmp/*); auto-rejecting"}, true},
		{"missing suffix", "! permission requested: external_directory (/tmp/*); approved", adapter.Event{}, false},
		{"missing colon", "! permission requested external_directory (/tmp/*); auto-rejecting", adapter.Event{}, false},
		{"missing patterns", "! permission requested: external_directory; auto-rejecting", adapter.Event{}, false},
		{"prose with markers", "a permission requested: none here; auto-rejecting, says the log", adapter.Event{}, false},
		{"noise", "warning: permission denied", adapter.Event{}, false},
	}
	a := &Adapter{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := a.ParseErrEvent([]byte(tc.line))
			if ok != tc.ok || got != tc.want {
				t.Errorf("ParseErrEvent = (%+v, %v), want (%+v, %v)", got, ok, tc.want, tc.ok)
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
