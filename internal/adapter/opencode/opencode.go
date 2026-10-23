// Package opencode drives OpenCode 1.19.0 headless: one `opencode run`
// per worker job, resumed by session id. Credentials remain in the CLI's
// own store (`opencode auth login`); the adapter supplies none.
package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
)

// Adapter drives the opencode CLI. Its zero value resolves opencode on PATH.
type Adapter struct {
	Binary string // empty means "opencode"
	Agent  string // optional --agent preset

	// model remembers the job's model from Command: limitReset reads a
	// Z.ai model's zone-less reset time in the provider's own zone, and
	// ParseEvent learns the model no other way. One adapter instance
	// serves one run at a time (a review runs a copy of the worker's
	// adapter), so the field is never shared between runs.
	model string
}

var _ adapter.Adapter = (*Adapter)(nil)

// DefaultModel names the CLI's own default, without passing a model flag.
const DefaultModel = "default"

func (a *Adapter) Name() string { return "opencode" }

// ProbeCommand implements adapter.Adapter: `opencode --version`, a cheap
// command that answers when the CLI is well (§8 "Health"), bound to ctx
// so a hung CLI is killed and reads as a failing probe.
func (a *Adapter) ProbeCommand(ctx context.Context) *exec.Cmd {
	binary := a.Binary
	if binary == "" {
		binary = "opencode"
	}
	return exec.CommandContext(ctx, binary, "--version")
}

// Command passes the prompt as one positional argument after --. OpenCode
// permits edits headless by default, matching worker policy, and
// workerPermissions limits bash to the job's mapped tool scope (none for a
// review) and denies paths outside the worktree.
// Never pass --auto (or its permission-bypass aliases); permission rules
// ride the environment, not the CLI's project config.
//
// OpenCode picks its directory as options.root ?? process.env.PWD ??
// process.cwd() and chdirs there (packages/cli/src/run/run.ts:74,
// run/v1.ts:29, mini.ts localDirectory), so cmd.Dir alone is not enough:
// the runner sets PWD to the worktree on every command it starts.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	if job.MCPConfig != "" {
		return nil, errors.New("opencode: the CLI run path has no MCP-config flag; a job carrying an MCP config cannot run on it")
	}
	if err := adapter.ValidateEffort("opencode", job.Effort); err != nil {
		return nil, fmt.Errorf("opencode: %w", err)
	}
	binary := a.Binary
	if binary == "" {
		binary = "opencode"
	}
	args := []string{"run", "--format", "json"}
	if job.Model != "" && job.Model != DefaultModel {
		args = append(args, "-m", job.Model)
	}
	if job.Effort != "" {
		// --variant is the CLI's provider-specific reasoning effort.
		args = append(args, "--variant", job.Effort)
	}
	if resumeSessionID != "" {
		args = append(args, "-s", resumeSessionID)
	}
	if a.Agent != "" {
		args = append(args, "--agent", a.Agent)
	}
	args = append(args, "--", job.Prompt)
	cmd := exec.Command(binary, args...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	a.model = job.Model
	cmd.Dir = workdir
	cmd.Env = workerPermissions(workerEnv(os.Environ()), job.ReadDirs, job.Tools, job.Review)
	return cmd, nil
}

// Keep the CLI's ambient login state, but never the runner's secrets (§5.1).
// OPENCODE_DISABLE_PROJECT_CONFIG makes the CLI skip the repository's
// opencode.json and .opencode/ (plugins, agents, commands): the user's
// global config and provider credentials stay in force.
func workerEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "BRIGADA_") && !strings.HasPrefix(kv, "OPENCODE_DISABLE_PROJECT_CONFIG=") {
			out = append(out, kv)
		}
	}
	return append(out, "OPENCODE_DISABLE_PROJECT_CONFIG=1")
}

// workerPermissions sets OPENCODE_PERMISSION so the CLI denies, rather than
// asks about, tool calls that reach outside the worktree. How the run path
// treats a permission (opencode v1.19.0, github.com/sst/opencode):
//
//   - A permission nothing decides defaults to "ask". On a headless
//     `opencode run` the ask is printed on stderr — "! permission
//     requested: <permission> (<patterns>); auto-rejecting", the "!" and
//     message wrapped in TEXT_WARNING_BOLD and TEXT_NORMAL colours — and
//     replied "reject"; the run loop itself carries on to the session's
//     idle, and the rejected call fails back to the model as
//     PermissionRejectedError (packages/opencode/src/cli/cmd/run.ts, the
//     permission.asked branch; UI.println writes stderr,
//     packages/opencode/src/cli/ui.ts; the rejection fails the tool's
//     deferred, packages/opencode/src/permission/index.ts).
//   - A rule whose action is "deny" never asks: the call fails back to the
//     model at once with PermissionDeniedError and the session runs on
//     (packages/opencode/src/permission/index.ts, the ask() deny branch;
//     rules reach every tool as the agent's merged ruleset,
//     packages/opencode/src/agent/agent.ts fromConfig on the config's
//     permission key, packages/opencode/src/session/tools.ts).
//   - OPENCODE_PERMISSION carries a JSON object merged into that config
//     key after every config file, and an invalid value is skipped with a
//     warning (packages/opencode/src/config/config.ts,
//     Flag.OPENCODE_PERMISSION). external_directory is one of its known
//     keys (packages/core/src/v1/config/permission.ts); upstream defaults
//     it to "ask" (opencode.ai/docs/permissions).
//
// So one deny rule turns an outside-the-worktree call into a plain tool
// error the model can answer — no ask, no auto-reject notice, no run that
// ends wordless. An ambient value keeps its other rules; an invalid one —
// which the CLI would discard — is replaced, never merged. Read dirs
// (§5.8) switch external_directory to its per-pattern form instead: an
// object of glob pattern to action where the last matching rule wins, so
// a "*" deny first and one allow per directory grant exactly those paths.
//
// bash is a per-command pattern object too, the last match winning, and
// the CLI checks each part of a compound command (a && b, a; b, $(...))
// and refuses it unless every part is allowed. bashRules maps the job's
// tool scope onto it; a review gets none.
func workerPermissions(env []string, readDirs []string, tools []string, review bool) []string {
	var rules map[string]any
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "OPENCODE_PERMISSION="); ok {
			_ = json.Unmarshal([]byte(v), &rules)
			break
		}
	}
	if rules == nil {
		rules = map[string]any{}
	}
	if len(readDirs) == 0 {
		rules["external_directory"] = "deny"
	} else {
		// The checkouts are read-only on disk, so allowing their patterns
		// cannot let a write through; everything else stays denied.
		patterns := map[string]any{"*": "deny"}
		for _, dir := range readDirs {
			patterns[dir] = "allow"
			patterns[dir+"/**"] = "allow"
		}
		rules["external_directory"] = patterns
	}
	rules["bash"] = bashRules(tools, review)
	if review {
		// A reviewer only reads: no edits, no network fetches.
		rules["edit"] = "deny"
		rules["webfetch"] = "deny"
	}
	value, err := json.Marshal(rules)
	if err != nil {
		return env // unreachable: a map of JSON values marshals
	}
	for i, kv := range env {
		if strings.HasPrefix(kv, "OPENCODE_PERMISSION=") {
			env[i] = "OPENCODE_PERMISSION=" + string(value)
			return env
		}
	}
	return append(env, "OPENCODE_PERMISSION="+string(value))
}

// bashRules maps a Claude-spelled tool scope onto opencode's bash
// permission: "deny" for a review, else an object with "*" denied first
// and `Bash(X:*)` allowing both "X" and "X *". A nil scope is the Go
// default; anything that does not map (other tools, odd forms, a
// wildcard in X) is left out, so it stays denied.
func bashRules(tools []string, review bool) any {
	if review {
		return "deny"
	}
	if tools == nil {
		tools = claude.DefaultWorkerTools()
	}
	rules := map[string]any{}
	for _, t := range tools {
		rest, ok := strings.CutPrefix(t, "Bash(")
		if !ok {
			continue
		}
		cmd, ok := strings.CutSuffix(rest, ":*)")
		if !ok || !bashCommandOK(cmd) {
			continue
		}
		rules[cmd] = "allow"
		rules[cmd+" *"] = "allow"
	}
	if len(rules) == 0 {
		return "deny"
	}
	rules["*"] = "deny" // sorts before every allowed command, so it is read first
	return rules
}

// bashCommandOK accepts a plain command prefix: it starts with a letter,
// digit, dot or slash and holds only letters, digits and . / _ - and
// spaces, so no wildcard, quote or shell metacharacter gets through.
func bashCommandOK(cmd string) bool {
	if cmd == "" || cmd != strings.TrimSpace(cmd) {
		return false
	}
	for i, r := range cmd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '/':
		case i > 0 && (r == '_' || r == '-' || r == ' '):
		default:
			return false
		}
	}
	return true
}

// The JSONL envelope is defined by the v1.19.0 CLI's cli/cmd/run.ts;
// nested parts and errors by sdk/js/src/v2/gen/types.gen.ts. This is
// deliberately not the server's message.part.updated/session.error stream.
// Declare only fields we read; unknown shapes are ignored, not guessed.
type lineJSON struct {
	Type      string     `json:"type"`
	SessionID string     `json:"sessionID"`
	Part      partJSON   `json:"part"`
	Error     *errorJSON `json:"error"`
}

type partJSON struct {
	Tokens json.RawMessage `json:"tokens"`
	Type   string          `json:"type"`
	Text   string          `json:"text"`
	Tool   string          `json:"tool"`
	State  struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
	} `json:"state"`
}

type errorJSON struct {
	Name string `json:"name"`
	Data struct {
		Message    string `json:"message"`
		StatusCode int    `json:"statusCode"`
	} `json:"data"`
}

// ParseEvent maps completed assistant text, tool states and provider errors.
// step_start supplies the session id without stealing a message/tool event.
// The CLI currently emits tool_use only for completed/error states, so real
// runs have tool ends without starts; do not invent starts from step_start
// (a model step is not a tool). If a running ToolPart is emitted, its schema
// maps directly to a start. Unknown tool states and event types stay ignored.
func (a *Adapter) ParseEvent(line []byte) (adapter.Event, bool) {
	var lj lineJSON
	if err := json.Unmarshal(line, &lj); err != nil {
		return adapter.Event{}, false
	}
	switch lj.Type {
	case "step_finish":
		u := adapter.ParseUsage(lj.Part.Tokens, "input", "output", "", "reasoning", "")
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(lj.Part.Tokens, &fields)
		cache := adapter.ParseUsage(fields["cache"], "", "", "read", "", "")
		if u == nil {
			u = cache
		} else {
			u.Add(cache)
		}
		// opencode's input excludes cache reads; fold them in so input is
		// all input, as for the other adapters.
		if cache != nil && cache.CachedInputTokens != nil {
			n := *cache.CachedInputTokens
			u.Add(&adapter.Usage{InputTokens: &n})
		}
		// Each step_finish that reports tokens closes one model request.
		if u != nil {
			u.Add(adapter.Calls(1))
		}
		return adapter.Event{Kind: adapter.EvUsage, Usage: u}, u != nil
	case "step_start":
		if lj.Part.Type == "step-start" && lj.SessionID != "" {
			return adapter.Event{Kind: adapter.EvSession, SessionID: lj.SessionID}, true
		}
	case "text":
		if lj.Part.Type == "text" && lj.Part.Text != "" {
			return adapter.Event{Kind: adapter.EvMessage, Text: lj.Part.Text}, true
		}
	case "tool_use":
		if lj.Part.Type != "tool" || lj.Part.Tool == "" {
			break
		}
		switch lj.Part.State.Status {
		case "running":
			return adapter.Event{Kind: adapter.EvToolStart, Text: lj.Part.Tool, Args: adapter.ToolArgs(lj.Part.State.Input)}, true
		case "completed", "error":
			// A tool failure is a finished call, not a provider failure.
			return adapter.Event{Kind: adapter.EvToolEnd, Text: lj.Part.Tool}, true
		}
	case "error":
		if lj.Error != nil && lj.Error.Name != "" {
			text := lj.Error.Data.Message
			if text == "" {
				text = lj.Error.Name
			}
			// Errors can precede step_start, so keep their envelope's id
			// as well as the failure: the runner must not lose either.
			ev := adapter.Event{Kind: adapter.EvError, Text: text, SessionID: lj.SessionID, ErrorKind: errorKind(*lj.Error)}
			if ev.ErrorKind == adapter.ErrLimit {
				ev.LimitUntil = limitReset(text, a.model)
			}
			return ev, true
		}
	}
	return adapter.Event{}, false
}

// ParseErrEvent recognizes the same JSON shapes on stderr, plus the one
// fixed-format line the headless run path prints when it auto-rejects a
// permission ask — never arbitrary prose containing words such as "quota"
// or "key".
func (a *Adapter) ParseErrEvent(line []byte) (adapter.Event, bool) {
	if ev, ok := autoRejectedPermission(line); ok {
		return ev, true
	}
	return a.ParseEvent(line)
}

// ansiRe strips the CLI's SGR colour codes, which it writes to stderr
// whether or not the stream is a TTY.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// autoRejectedPermission recognizes the ask notice `opencode run` prints
// on stderr when nothing answered a permission, whatever --format says:
//
//	! permission requested: external_directory (/worktree-outside/*); auto-rejecting
//
// with the "!" in TEXT_WARNING_BOLD and the message reset to TEXT_NORMAL.
// The rejected call fails back to the model, so the run may still answer;
// the event is ErrOther — a plain failure, not a limit or an auth one —
// and carries the line's own text, so a run that then ends without a word
// says why instead of a bare no-result.
func autoRejectedPermission(line []byte) (adapter.Event, bool) {
	const prefix = "permission requested: "
	const suffix = "; auto-rejecting"
	text := strings.TrimSpace(ansiRe.ReplaceAllString(string(line), ""))
	text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "!"))
	msg, ok := strings.CutPrefix(text, prefix)
	if !ok || !strings.HasSuffix(msg, suffix) {
		return adapter.Event{}, false
	}
	msg = strings.TrimSuffix(msg, suffix)
	// run.ts always renders "<permission> (<patterns>)"; hold the line to
	// that shape rather than matching any prose with the two markers.
	if !strings.Contains(msg, " (") || !strings.HasSuffix(msg, ")") {
		return adapter.Event{}, false
	}
	return adapter.Event{Kind: adapter.EvError, ErrorKind: adapter.ErrOther, Text: prefix + msg + suffix}, true
}

type errorPhrase struct {
	kind   adapter.ErrorKind
	phrase string
}

var errorPhrases = []errorPhrase{
	{adapter.ErrLimit, "429"},
	{adapter.ErrLimit, "rate limit"},
	{adapter.ErrLimit, "rate-limit"},
	{adapter.ErrLimit, "usage limit"},
	{adapter.ErrLimit, "usage-limit"},
	{adapter.ErrLimit, "quota"},
	{adapter.ErrLimit, "limit exhausted"},
	{adapter.ErrLimit, "too many requests"},
	{adapter.ErrAuth, "401"},
	{adapter.ErrAuth, "403"},
	{adapter.ErrAuth, "unauthorized"},
	{adapter.ErrAuth, "forbidden"},
	{adapter.ErrAuth, "invalid api key"},
	{adapter.ErrAuth, "invalid_api_key"},
	{adapter.ErrAuth, "invalid key"},
	{adapter.ErrAuth, "invalid-key"},
	{adapter.ErrAuth, "authentication"},
}

// now is the clock limitReset reads; tests replace it.
var now = time.Now

// localZone is the zone a reset time without its own zone is read in:
// the machine's own. That is an assumption, since the provider's text
// names no zone; if the guess is early, the engine re-cools on the next
// limit error. It is spelled through LoadLocation because the leak
// guard's host rule would take the zone's usual spelling for a .local
// hostname. Tests replace the variable.
var localZone = func() *time.Location {
	loc, err := time.LoadLocation("Local")
	if err != nil {
		return time.UTC // unreachable: "Local" always resolves
	}
	return loc
}()

// zaiZone is the zone a Z.ai reset time without its own zone is read in:
// the provider's message is Beijing wall-clock (UTC+8), whatever the
// machine's zone.
var zaiZone = time.FixedZone("UTC+8", 8*3600)

// zaiModel reports whether the opencode model id is served by Z.ai: the
// coding-plan routes and the zhipuai ones. Their limit messages print a
// zone-less reset time in Beijing wall-clock.
func zaiModel(model string) bool {
	return strings.HasPrefix(model, "zai-coding-plan/") || strings.HasPrefix(model, "zhipuai")
}

// limitResetRe is the provider's reset note: "Your limit will reset at
// 2026-10-20 21:32:04", seconds optional, optionally followed by a zone:
// Z, UTC, GMT or an offset such as +08:00.
var limitResetRe = regexp.MustCompile(`reset at (\d{4})-(\d{2})-(\d{2})[ t](\d{2}):(\d{2})(?::(\d{2}))?(?:\s*(z|utc|gmt|[+-]\d{2}(?::?\d{2})?))?(?:[^a-z0-9]|$)`)

// limitWindowRe is a window a limit message names: "for 5 hour",
// "for 30 minute", "for 2 day".
var limitWindowRe = regexp.MustCompile(`for (\d+) (second|minute|hour|day)s?\b`)

// limitWindow reads the window a limit message names, as a duration;
// false when it names none.
func limitWindow(text string) (time.Duration, bool) {
	m := limitWindowRe.FindStringSubmatch(strings.ToLower(text))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch strings.TrimSuffix(m[2], "s") {
	case "second":
		return time.Duration(n) * time.Second, true
	case "minute":
		return time.Duration(n) * time.Minute, true
	case "day":
		return time.Duration(n) * 24 * time.Hour, true
	default: // "hour"
		return time.Duration(n) * time.Hour, true
	}
}

// limitReset reads the reset time out of a limit message, as an RFC3339
// instant. Best effort — anything that does not parse, or is already
// past, is "" (unknown), never an error. A zone-less time is read in the
// machine's zone, except on a Z.ai model, whose provider prints Beijing
// wall-clock. When the message names a window ("for 5 hour"), a reset
// further away than that window is clamped to now + window: the window,
// not the wall clock, is what the provider enforces.
func limitReset(text, model string) string {
	m := limitResetRe.FindStringSubmatch(strings.ToLower(text))
	if m == nil {
		return ""
	}
	var n [6]int
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1]) // m[6] may be empty: 0 seconds
	}
	loc := localZone
	if m[7] == "" && zaiModel(model) {
		loc = zaiZone
	}
	switch z := m[7]; {
	case z == "":
	case z[0] == 'z' || z[0] == 'u' || z[0] == 'g':
		loc = time.UTC
	default:
		digits := strings.ReplaceAll(z[1:], ":", "")
		h, _ := strconv.Atoi(digits[:2])
		min := 0
		if len(digits) == 4 {
			min, _ = strconv.Atoi(digits[2:])
		}
		if h > 14 || min > 59 {
			return ""
		}
		off := h*3600 + min*60
		if z[0] == '-' {
			off = -off
		}
		loc = time.FixedZone("", off)
	}
	if n[1] < 1 || n[1] > 12 || n[2] < 1 || n[2] > 31 || n[3] > 23 || n[4] > 59 || n[5] > 59 {
		return ""
	}
	clock := now()
	t := time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], 0, loc)
	if t.Day() != n[2] || !t.After(clock) {
		return ""
	}
	if w, ok := limitWindow(text); ok {
		if limit := clock.Add(w); t.After(limit) {
			t = limit
		}
	}
	return t.Format(time.RFC3339)
}

func errorKind(e errorJSON) adapter.ErrorKind {
	switch e.Data.StatusCode {
	case 429:
		return adapter.ErrLimit
	case 401, 403:
		return adapter.ErrAuth
	}
	if e.Name == "ProviderAuthError" {
		return adapter.ErrAuth
	}
	lower := strings.ToLower(e.Data.Message)
	for _, p := range errorPhrases {
		if strings.Contains(lower, p.phrase) {
			return p.kind
		}
	}
	return adapter.ErrOther
}

// ResultText is the run's last assistant message, which the worker/review
// contract reads (§5.4). Tool, session, reasoning and usage events cannot
// replace it, and earlier assistant messages are never concatenated into it.
func (a *Adapter) ResultText(events []adapter.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == adapter.EvMessage {
			return events[i].Text
		}
	}
	return ""
}
