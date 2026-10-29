package claude

// The foreman's read scope (§4.1): the session's file tools — Read,
// Grep and Glob — read exactly one root, the reading checkout the
// session runs in, and are refused everywhere else. Permission rules
// cannot say that: their patterns are single paths with no negation,
// and a deny broad enough to cover the directories around the root
// covers the root too. The confinement is therefore a PreToolUse hook
// the session's settings carry: the hook runs before every call of the
// three tools — every agent's call, a subagent's included — and its
// exit-2 refusal is a block no allow rule lifts, not one the user's own
// settings, which an attach loads, can widen. The hook command is
// brigada itself, the readscope subcommand, with the allowed root as
// its one argument; cmd/brigada dispatches it.
//
// The hook fails closed: the CLI treats any PreToolUse exit other than
// 2 as non-blocking, so an unreadable payload, one that cannot be
// parsed, or a matched tool call with no path argument to judge is
// refused, never waved through. The settings also pin
// disableAllHooks to false — the user's own settings, loaded by an
// attach and by a headless turn alike, could switch every hook off —
// and the flag's settings outrank them.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadScopeSubcommand is the brigada subcommand the read-scope hook
// runs. The session's settings name it; cmd/brigada dispatches it
// beside the command table, because no one calls it by hand.
const ReadScopeSubcommand = "readscope"

// readScopeMatcherPattern is the hook's tool matcher: the three file
// tools, anchored, so NotebookRead — which ends in Read and is denied
// outright in the foreman's tool scope — does not match.
const readScopeMatcherPattern = "^(Read|Grep|Glob)$"

// foremanSettingsJSON renders the settings a foreman session runs under
// (§4.1, §4.2 item 1): the read-scope hook on the file tools — the same
// for a headless turn and for an attach — with the attach's status line
// beside it when one is set, and disableAllHooks pinned to false: the
// user's own settings, which both a headless turn (--setting-sources
// user) and an attach load, could otherwise switch the hook off with a
// single key, and the flag's settings outrank theirs. root is the
// session's allowed root, its working directory: the project's reading
// checkout, never brigada's own state, whose tokens and per-turn
// configs are not the foreman's to read.
func foremanSettingsJSON(root, statusLine string) (string, error) {
	command, err := readScopeHookCommand(root)
	if err != nil {
		return "", err
	}
	settings := foremanSettings{
		DisableAllHooks: false,
		Hooks: &foremanHooks{
			PreToolUse: []readScopeGroup{{
				Matcher: readScopeMatcherPattern,
				Hooks:   []readScopeHook{{Type: "command", Command: command}},
			}},
		},
	}
	if statusLine != "" {
		settings.StatusLine = &statusLineSettings{
			Type:            "command",
			Command:         statusLine,
			RefreshInterval: statusLineRefreshSeconds,
		}
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("encode the foreman session's settings: %w", err)
	}
	return string(encoded), nil
}

// foremanSettings is the settings JSON a foreman session carries:
// hooks switched on for the session whatever the user's own settings
// say, the read-scope hook always, the status line on an attach.
type foremanSettings struct {
	DisableAllHooks bool                `json:"disableAllHooks"`
	Hooks           *foremanHooks       `json:"hooks,omitempty"`
	StatusLine      *statusLineSettings `json:"statusLine,omitempty"`
}

// foremanHooks is the settings' hooks table; the read scope is a
// PreToolUse hook, run before each matched tool call.
type foremanHooks struct {
	PreToolUse []readScopeGroup `json:"PreToolUse"`
}

// readScopeGroup is one matcher with its hooks: the file tools, and the
// one command hook that checks them.
type readScopeGroup struct {
	Matcher string          `json:"matcher"`
	Hooks   []readScopeHook `json:"hooks"`
}

// readScopeHook is the one command of the read scope.
type readScopeHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// statusLineSettings is the status bar entry of an attached session's
// settings (§10): the command the bar runs and how often it re-runs
// while the chat is idle.
type statusLineSettings struct {
	Type            string `json:"type"`
	Command         string `json:"command"`
	RefreshInterval int    `json:"refreshInterval"`
}

// readScopeHookCommand builds the hook's shell command: brigada's own
// binary as the readscope subcommand, with the allowed root beside it,
// absolute and cleaned. The binary is the process brigada runs in — the
// runner for a headless turn, the attach client for a chat — the same
// one whose settings carry the hook. A binary or root that cannot be
// named is an error, never a session without the hook.
func readScopeHookCommand(root string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find brigada's own binary for the read-scope hook: %w", err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("make the read scope's root absolute: %w", err)
	}
	return shellQuote(exe) + " " + ReadScopeSubcommand + " --root " + shellQuote(filepath.Clean(abs)), nil
}

// shellQuote quotes s for the shell the CLI runs hook commands with:
// single quotes around it, each single quote inside closed, escaped and
// reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ReadScopeDeny decides one PreToolUse payload for the read scope: it
// reports whether the call's path arguments stay inside root, and the
// reason to refuse the call with when one does not. It fails closed —
// the CLI treats any exit other than 2 as non-blocking, so a payload
// that is not JSON, names no tool, names a tool the matcher does not
// match, or cannot be read into path arguments at all is refused: the
// scope is a boundary, and what cannot be judged does not pass. The
// one exception is a Grep or Glob with no path argument: they search
// the working directory, which is the root itself.
//
// Each path argument is read the way the CLI would read it: absolute,
// or against the payload's own working directory, with symlinks
// evaluated — so a path that reaches outside the root through a link,
// a .. segment or another spelling of the same directory (macOS /var
// against /private/var) is caught all the same.
func ReadScopeDeny(root string, payload []byte) (reason string, deny bool) {
	var call hookCall
	if json.Unmarshal(payload, &call) != nil || call.ToolName == "" {
		return "brigada: the call cannot be read against the read scope; refusing it", true
	}
	base := call.Cwd
	if base == "" {
		base = root
	}
	scope := resolveScopePath(root, root)
	paths, ok := readScopePaths(call.ToolName, call.ToolInput)
	if !ok {
		return fmt.Sprintf("brigada: the %s call's paths cannot be read against the read scope (%s); refusing it", call.ToolName, scope), true
	}
	for _, arg := range paths {
		if arg == "" {
			continue
		}
		resolved := resolveScopePath(base, arg)
		if resolved != scope && !strings.HasPrefix(resolved, scope+string(filepath.Separator)) {
			return fmt.Sprintf("brigada: %s is outside the foreman's read scope (%s); the foreman reads only its checkout", resolved, scope), true
		}
	}
	return "", false
}

// hookCall is the PreToolUse payload the CLI feeds the hook on stdin:
// the tool's name, the directory the session runs in, and the call's
// own arguments.
type hookCall struct {
	ToolName  string          `json:"tool_name"`
	Cwd       string          `json:"cwd"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// toolPathArgs is the path-carrying arguments of one file tool call:
// Read's file or notebook, Grep's and Glob's search path, and Glob's
// pattern, whose literal head is a path the rest is matched under.
type toolPathArgs struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Path         string `json:"path"`
	Pattern      string `json:"pattern"`
}

// readScopePaths lists the path arguments one file tool call reads, and
// whether the call could be read at all: false refuses it. A tool the
// matcher does not name, an input that is not a JSON object, and a Read
// with no file to name are all unreadable — a Grep or a Glob with no
// path is not, for it searches the working directory, the root itself.
func readScopePaths(tool string, input json.RawMessage) ([]string, bool) {
	if tool != "Read" && tool != "Grep" && tool != "Glob" {
		return nil, false
	}
	var args toolPathArgs
	if len(input) == 0 || json.Unmarshal(input, &args) != nil {
		return nil, false
	}
	switch tool {
	case "Read":
		if args.FilePath == "" && args.NotebookPath == "" {
			return nil, false
		}
		return []string{args.FilePath, args.NotebookPath}, true
	case "Grep":
		return []string{args.Path}, true
	default: // Glob: the pattern's literal head is a path too
		return []string{args.Path, globLiteralHead(args.Pattern)}, true
	}
}

// globLiteralHead cuts a glob pattern at its first metacharacter and
// drops a head that names nothing beyond the working directory: what is
// left is a literal path the pattern's matches live under.
func globLiteralHead(pattern string) string {
	if i := strings.IndexAny(pattern, "*?[{"); i >= 0 {
		pattern = pattern[:i]
	}
	if pattern == "" || pattern == "." || pattern == "./" {
		return ""
	}
	return pattern
}

// resolveScopePath is p as the CLI would read it: absolute — against
// base when it is not — cleaned, and with symlinks evaluated. A path
// that does not exist keeps its missing tail and evaluates the deepest
// directory that does exist, so a missing file is judged where it
// sits, not read as the raw spelling.
func resolveScopePath(base, p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, strings.TrimPrefix(p, dir))
		}
		if dir == string(filepath.Separator) || dir == "." || dir == "" {
			return p
		}
	}
}
