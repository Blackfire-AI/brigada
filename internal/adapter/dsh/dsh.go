// Package dsh drives DeepSeek Harness (npm @deepseek-ai/dsh, verified on
// 0.2.1-rc.2) headless: one `dsh --profile headless --json` per worker job,
// the prompt on stdin, resumed by session id. The adapter is experimental,
// as the CLI is a developer preview. Credentials remain the CLI's own: the
// ambient DEEPSEEK_API_KEY or dsh's credential store; the adapter supplies
// none.
package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Adapter drives the dsh CLI. Its zero value resolves dsh on PATH.
type Adapter struct {
	Binary string // empty means "dsh"
}

var _ adapter.Adapter = (*Adapter)(nil)

// DefaultModel is dsh's own default model, named explicitly because the
// per-job patch file always carries a model.
const DefaultModel = "deepseek-flash"

func (a *Adapter) Name() string { return "dsh" }

// ProbeCommand implements adapter.Adapter: `dsh --version`, a cheap
// command that answers when the CLI is well (§8 "Health"), bound to ctx
// so a hung CLI is killed and reads as a failing probe.
func (a *Adapter) ProbeCommand(ctx context.Context) *exec.Cmd {
	binary := a.Binary
	if binary == "" {
		binary = "dsh"
	}
	return exec.CommandContext(ctx, binary, "--version")
}

// Command runs `dsh --profile headless --patch <file> --json [--session-id
// <id>] -` with the prompt on stdin. Env, set on top of the ambient
// environment (DEEPSEEK_API_KEY included; BRIGADA_* dropped):
//
//   - DSH_TELEMETRY_MODE=DISABLED: the headless profile otherwise sends
//     session telemetry to DeepSeek.
//   - DSH_PERMISSION_MODE=read-only for a review job, workspace-write for
//     work and fix jobs. Read-only denies writes (bash writes included) at
//     the OS level and refuses escalation headless; workspace-write allows
//     the working directory and the temp directory and denies elsewhere.
//     danger-full-access is never set.
//   - For work and fix jobs, GOCACHE in a brigada directory under the temp
//     directory, shared across jobs, because the sandbox denies new entries
//     in the user's go-build cache; and GIT_OPTIONAL_LOCKS=0, so git status
//     and diff take no index.lock, which the sandbox denies in the runner
//     mirror's gitdir.
//
// The model and the reasoning effort ride a per-job patch file in a fresh
// temporary directory outside the worktree. The Adapter interface has no
// hook for the end of the command's process, so the file is left in the OS
// temp directory; it holds no secret.
func (a *Adapter) Command(job adapter.Job, workdir, resumeSessionID string) (*exec.Cmd, error) {
	if job.MCPConfig != "" {
		return nil, errors.New("dsh: no MCP-config flag is known for the headless profile; a job carrying an MCP config cannot run on it")
	}
	if err := adapter.ValidateEffort("dsh", job.Effort); err != nil {
		return nil, fmt.Errorf("dsh: %w", err)
	}
	binary := a.Binary
	if binary == "" {
		binary = "dsh"
	}
	patch, err := writePatch(job.Model, job.Effort)
	if err != nil {
		return nil, fmt.Errorf("dsh: %w", err)
	}
	args := []string{"--profile", "headless", "--patch", patch, "--json"}
	if resumeSessionID != "" {
		args = append(args, "--session-id", resumeSessionID)
	}
	args = append(args, "-")
	cmd := exec.Command(binary, args...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	cmd.Dir = workdir
	cmd.Stdin = strings.NewReader(job.Prompt)
	cmd.Env = workerEnv(os.Environ(), job.Review)
	return cmd, nil
}

// writePatch writes the per-job patch file and returns its path. An empty
// model is dsh's default; an empty effort leaves the provider's default.
func writePatch(model, effort string) (string, error) {
	if model == "" {
		model = DefaultModel
	}
	dir, err := os.MkdirTemp("", "brigada-dsh-")
	if err != nil {
		return "", err
	}
	// JSON strings are valid YAML scalars, so a quoted value cannot break
	// out of its line.
	quoted, err := json.Marshal(model)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("- id: agent-default-model\n  config:\n    provider: deepseek-official\n")
	b.WriteString("    model: " + string(quoted) + "\n")
	if effort != "" {
		quotedEffort, err := json.Marshal(effort)
		if err != nil {
			return "", err
		}
		b.WriteString("    reasoningEffort: " + string(quotedEffort) + "\n")
	}
	path := filepath.Join(dir, "patch.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// workerEnv keeps the CLI's ambient login state, DEEPSEEK_API_KEY among it,
// but never the runner's secrets (§5.1), and pins the two DSH_ variables
// the policy sets, replacing any ambient value.
func workerEnv(env []string, review bool) []string {
	mode := "workspace-write"
	if review {
		mode = "read-only"
	}
	out := make([]string, 0, len(env)+4)
	for _, kv := range env {
		if strings.HasPrefix(kv, "BRIGADA_") || strings.HasPrefix(kv, "DSH_TELEMETRY_MODE=") || strings.HasPrefix(kv, "DSH_PERMISSION_MODE=") {
			continue
		}
		if !review && (strings.HasPrefix(kv, "GOCACHE=") || strings.HasPrefix(kv, "GIT_OPTIONAL_LOCKS=")) {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "DSH_TELEMETRY_MODE=DISABLED", "DSH_PERMISSION_MODE="+mode)
	if !review {
		out = append(out, "GOCACHE="+filepath.Join(os.TempDir(), "brigada-gocache"), "GIT_OPTIONAL_LOCKS=0")
	}
	return out
}

// The JSONL envelope is what the 0.2.1-rc.2 spike printed. Declare only
// fields we read; unknown shapes are ignored, not guessed.
type lineJSON struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Phase     string          `json:"phase"`
	Usage     json.RawMessage `json:"usage"`
	Reason    *struct {
		Kind  string     `json:"kind"`
		Error *errorJSON `json:"error"`
	} `json:"reason"`
	Text   string          `json:"text"`
	Tool   string          `json:"tool"`
	CallID string          `json:"callId"`
	Input  json.RawMessage `json:"input"`
}

type errorJSON struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Status  int    `json:"status"`
}

// ParseEvent maps the session id, step usage, assistant text and final
// message, tool calls and results, and a failed turn. Thinking and the
// other status phases are not tracked.
func (a *Adapter) ParseEvent(line []byte) (adapter.Event, bool) {
	var lj lineJSON
	if err := json.Unmarshal(line, &lj); err != nil {
		return adapter.Event{}, false
	}
	switch lj.Type {
	case "session":
		if lj.SessionID != "" {
			return adapter.Event{Kind: adapter.EvSession, SessionID: lj.SessionID}, true
		}
	case "status":
		switch lj.Phase {
		case "step_end":
			u := stepUsage(lj.Usage)
			return adapter.Event{Kind: adapter.EvUsage, Usage: u}, u != nil
		case "turn_end":
			if lj.Reason != nil && lj.Reason.Kind == "error" {
				var e errorJSON
				if lj.Reason.Error != nil {
					e = *lj.Reason.Error
				}
				text := e.Message
				if text == "" {
					text = "the turn ended in error"
				}
				return adapter.Event{Kind: adapter.EvError, Text: text, ErrorKind: errorKind(e)}, true
			}
		}
	case "text", "final":
		if lj.Text != "" {
			return adapter.Event{Kind: adapter.EvMessage, Text: lj.Text}, true
		}
	case "tool_call":
		if lj.Tool != "" {
			return adapter.Event{Kind: adapter.EvToolStart, Text: lj.Tool, Args: adapter.ToolArgs(lj.Input)}, true
		}
	case "tool_result":
		// A tool failure is a finished call, not a provider failure.
		return adapter.Event{Kind: adapter.EvToolEnd, Text: lj.CallID}, true
	}
	return adapter.Event{}, false
}

// stepUsage reads a step_end usage. dsh's inputTokens excludes the cache
// (the spike's totalTokens is input + output + cacheRead), so the cache
// reads and writes are added to the input, as the claude adapter does for
// Anthropic's counts, and the reads are also reported as cached.
func stepUsage(raw json.RawMessage) *adapter.Usage {
	u := adapter.ParseUsage(raw, "inputTokens", "outputTokens", "cacheReadTokens", "", "")
	write := adapter.ParseUsage(raw, "cacheWriteTokens", "", "", "", "")
	if write != nil {
		if u == nil {
			u = &adapter.Usage{}
		}
		u.Add(write)
	}
	if u != nil && u.CachedInputTokens != nil {
		n := *u.CachedInputTokens
		u.Add(&adapter.Usage{InputTokens: &n})
	}
	return u
}

// errLineRe is the stderr line a failed run prints: `dsh: <CODE>: <message>`.
// The code shape keeps prose that merely starts with "dsh: " out.
var errLineRe = regexp.MustCompile(`^dsh: ([A-Z][A-Z0-9_]*): (.+)$`)

// ParseErrEvent recognizes the CLI's stderr error line, and the same JSON
// shapes as stdout; any other stderr line carries nothing to act on.
func (a *Adapter) ParseErrEvent(line []byte) (adapter.Event, bool) {
	if m := errLineRe.FindStringSubmatch(strings.TrimSpace(string(line))); m != nil {
		e := errorJSON{Code: m[1], Message: m[2]}
		return adapter.Event{Kind: adapter.EvError, Text: e.Message, ErrorKind: errorKind(e)}, true
	}
	ev, ok := a.ParseEvent(line)
	if ok && ev.Kind != adapter.EvError {
		return adapter.Event{}, false
	}
	return ev, ok
}

// errorKind sorts a dsh error. The auth cases were reproduced (a missing
// key, an invalid key); the limit cases were not and are best effort: the
// HTTP statuses DeepSeek uses for rate limits (429) and insufficient
// balance (402), and wording about either.
func errorKind(e errorJSON) adapter.ErrorKind {
	if e.Code == "MISSING_CREDENTIAL" || e.Code == "AUTH" || e.Status == 401 {
		return adapter.ErrAuth
	}
	if e.Status == 429 || e.Status == 402 {
		return adapter.ErrLimit
	}
	lower := strings.ToLower(e.Message)
	for _, phrase := range limitPhrases {
		if strings.Contains(lower, phrase) {
			return adapter.ErrLimit
		}
	}
	if adapter.IsInfraText(e.Message) {
		return adapter.ErrInfra
	}
	return adapter.ErrOther
}

var limitPhrases = []string{
	"rate limit",
	"rate-limit",
	"ratelimit",
	"too many requests",
	"insufficient balance",
}

// ResultText is the final event's text; a run that printed none falls back
// to its last text event. Both are messages, and the final one comes last,
// so the last message is the answer. Tool, session, thinking and usage
// events cannot replace it, and earlier messages are never concatenated.
func (a *Adapter) ResultText(events []adapter.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == adapter.EvMessage {
			return events[i].Text
		}
	}
	return ""
}
