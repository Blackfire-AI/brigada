package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/caps"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/notify"
)

// maxSocketPath is the longest Unix socket path the kernel accepts:
// sockaddr_un.sun_path holds 104 bytes on the tightest system (108 on
// Linux), and a bind past the cap fails with a bare "invalid argument"
// that names nothing. Paths at or over the cap are refused up front,
// before anything is started, with a message that says what to do.
const maxSocketPath = 104

// DefaultStateDir is where the engine's state lives when nothing names
// another place (§2): $XDG_STATE_HOME/brigada, else
// ~/.local/state/brigada.
func DefaultStateDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "brigada")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "brigada" // no home to build on; relative, and the caller decides
	}
	return filepath.Join(home, ".local", "state", "brigada")
}

// Paths are the engine's files inside one state directory: the store,
// the API's socket, the single-instance lock beside the store (§13), the
// token file (§6.4) and the log a detached engine writes.
type Paths struct {
	StateDir string
	Store    string
	Socket   string
	Lock     string
	Tokens   string
	Log      string
}

// PathsOf derives every path from stateDir.
func PathsOf(stateDir string) Paths {
	return Paths{
		StateDir: stateDir,
		Store:    filepath.Join(stateDir, "store.db"),
		Socket:   filepath.Join(stateDir, "brigada.sock"),
		Lock:     filepath.Join(stateDir, "engine.lock"),
		Tokens:   filepath.Join(stateDir, "tokens"),
		Log:      filepath.Join(stateDir, "engine.log"),
	}
}

// CheckSocketPath refuses a socket path the kernel could not bind,
// naming the length and the cap so the fix — a shorter state directory —
// is on the line.
func CheckSocketPath(path string) error {
	if len(path) >= maxSocketPath {
		return fmt.Errorf("socket path %s is %d bytes; a Unix socket path is capped at %d bytes (sun_path); use a shorter state directory",
			path, len(path), maxSocketPath)
	}
	return nil
}

// FileConfig is the user's config file (§2): the settings that do not
// change per invocation. One `key = value` per line; `#` comments and
// blank lines allowed; unknown keys are refused, so a typo cannot
// silently tune nothing.
type FileConfig struct {
	StateDir    string
	IdleMinutes float64 // 0 = the default
	Notify      string  // notify = auto|none (§10); empty = auto
	// NotifyWebhook is the optional webhook URL (notify.webhook, §10):
	// one JSON POST per delivered notification. Empty = none. The URL
	// may carry a token, so it lives in this file alone — never in a
	// project's own files — and never reaches a log, status or error.
	NotifyWebhook string
	Caps          caps.Limits // concurrency caps, cost weights and tier overrides (§8.1)
	// Rounds are the default per-task round caps (rounds.fix,
	// rounds.review, rounds.restart); a key the file never names stays nil
	// and keeps the engine default.
	Rounds RoundsConfig
	// Review is the default tier, effort and family rule of a review
	// (review.tier, review.effort, review.family, §7, §8); a key the file
	// never names stays empty and keeps the engine default.
	Review engine.ReviewConfig
	// SnapshotDir is where the engine snapshots its store daily (§6.4):
	// an absolute directory outside the state directory's store. Empty =
	// no snapshots.
	SnapshotDir string
	// TaskAutoRetry controls one automatic restart after a transient work
	// failure. Nil keeps the engine default, on.
	TaskAutoRetry *bool
	// ForemanWakesPerDay is the default daily foreman wake budget of
	// every project (§4.3); 0 keeps the built-in default. A project's
	// foreman_wakes_per_day setting overrides it.
	ForemanWakesPerDay int
	// DefaultReviewer is the harness a standalone review (§7) goes to when
	// its author's family cannot be told. Empty = none.
	DefaultReviewer string
	// Tools is the default claude worker tool scope per "harness/model"
	// (§5.2, tools.<harness>/<model>): the parsed patterns, an empty
	// non-nil list for an explicit none. A pair the file never names has
	// no entry, which is the Go default and not none.
	Tools map[string][]string
	// Env holds the references of env.<harness>.<VAR> (§5.1), harness then
	// variable: keychain:<item> or file:<path>. Never a value.
	Env map[string]map[string]string
}

// RoundsConfig holds the round caps the config file names; nil = unnamed.
type RoundsConfig struct {
	Fix, Review, Restart *int
}

// Caps resolves the file's round caps over the engine defaults.
func (r RoundsConfig) Caps() engine.RoundCaps {
	c := engine.DefaultConfig().RoundCaps
	if r.Fix != nil {
		c.Fix = *r.Fix
	}
	if r.Review != nil {
		c.Review = *r.Review
	}
	if r.Restart != nil {
		c.Restart = *r.Restart
	}
	return c
}

// ParseTools parses a tool scope in its flag and config spelling: Claude
// Code tool patterns separated by "+", or "none" for the file tools
// alone. The result is non-nil for none, so a caller can tell an
// explicit none from no scope at all. A pattern containing a comma
// cannot ride an --agent value, which splits on commas.
func ParseTools(value string) ([]string, error) {
	return adapter.ParseToolScope(value)
}

// ToolsRefusal is why a harness other than claude takes no tools scope,
// naming the sandbox model it runs under instead (read from each
// adapter): brigada scopes the claude CLI's tools only.
func ToolsRefusal(harness string) string {
	switch harness {
	case "codex":
		return "the codex harness takes no tools scope: workers run `codex exec -s workspace-write`, a sandbox that lets them edit inside the worktree and keeps the network off by default"
	case "kimi":
		return "the kimi harness takes no tools scope: workers run headless with -p, which takes no approval prompts, so brigada has no tool list to narrow"
	case "opencode":
		return "the opencode harness takes no tools scope: headless `opencode run` permits edits, and brigada sets its bash permission from the project's worker_tools, so an agent's own tools= has no effect"
	default:
		return fmt.Sprintf("the %s harness takes no tools scope: only the claude harness has a tool list", harness)
	}
}

// DefaultConfigPath is where the config file lives when nothing names
// another place: $XDG_CONFIG_HOME/brigada/config, else
// ~/.config/brigada/config.
func DefaultConfigPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "brigada", "config")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join("brigada", "config")
	}
	return filepath.Join(home, ".config", "brigada", "config")
}

// LoadConfigFile reads the config file. A missing file is the empty
// config, not an error: most installs have none.
func LoadConfigFile(path string) (FileConfig, error) {
	return loadConfigFile(path, false)
}

// LoadRequiredConfigFile reads a config file explicitly named by the caller.
// Missing explicit files are errors: falling back could select unintended state.
func LoadRequiredConfigFile(path string) (FileConfig, error) {
	return loadConfigFile(path, true)
}

func loadConfigFile(path string, required bool) (FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return FileConfig{}, nil
		}
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return FileConfig{}, fmt.Errorf("config %s: line %d: want `key = value`, got no `=`", path, n+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		var err error
		switch {
		case key == "state_dir":
			cfg.StateDir = value
		case key == "idle_minutes":
			cfg.IdleMinutes, err = strconv.ParseFloat(value, 64)
			if err == nil && cfg.IdleMinutes < 0 {
				err = fmt.Errorf("negative")
			}
		case key == "notify":
			switch value {
			case notify.ModeAuto, notify.ModeNone:
				cfg.Notify = value
			default:
				err = fmt.Errorf("want %s or %s", notify.ModeAuto, notify.ModeNone)
			}
		case key == "notify.webhook":
			err = notify.ValidWebhookURL(value)
			if err == nil {
				cfg.NotifyWebhook = value
			}
		case key == "snapshot_dir":
			if value != "" && !filepath.IsAbs(value) {
				err = fmt.Errorf("want an absolute directory, got %q", value)
			}
			cfg.SnapshotDir = value
		case key == "headroom":
			var on bool
			on, err = strconv.ParseBool(value)
			cfg.Caps.NoHeadroom = !on
		case key == "default_reviewer":
			cfg.DefaultReviewer = value
		case key == "task_auto_retry":
			var on bool
			on, err = strconv.ParseBool(value)
			cfg.TaskAutoRetry = &on
		case key == "foreman_wakes_per_day":
			var n int
			n, err = strconv.Atoi(value)
			if err == nil && (n < 1 || n > engine.MaxForemanWakesPerDay) {
				err = fmt.Errorf("want an integer from 1 to %d", engine.MaxForemanWakesPerDay)
			}
			cfg.ForemanWakesPerDay = n
		case strings.HasPrefix(key, "rounds."):
			err = setRounds(&cfg.Rounds, key, value)
		case key == "review.tier":
			if !engine.ValidReviewTier(value) {
				err = fmt.Errorf("want %s or %s", engine.ReviewTierBelow, engine.ReviewTierSame)
			}
			cfg.Review.Tier = value
		case key == "review.effort":
			err = adapter.ValidateTaskEffort(value)
			if err == nil && value == "" {
				err = fmt.Errorf("want one of %s", strings.Join(adapter.TaskEffortLevels, ", "))
			}
			cfg.Review.Effort = value
		case key == "review.family":
			if !engine.ValidReviewFamily(value) {
				err = fmt.Errorf("want %s or %s", engine.ReviewFamilyPrefer, engine.ReviewFamilyRequire)
			}
			cfg.Review.Family = value
		case strings.HasPrefix(key, "caps."), strings.HasPrefix(key, "cost."), strings.HasPrefix(key, "tier."):
			err = setCap(&cfg.Caps, key, value)
		case strings.HasPrefix(key, "tools."):
			err = setTools(&cfg, key, value)
		case strings.HasPrefix(key, "env."):
			err = setEnvRef(&cfg, key, value)
		default:
			return FileConfig{}, fmt.Errorf("config %s: line %d: unknown key %q; want state_dir, idle_minutes, notify, notify.webhook, snapshot_dir, headroom, task_auto_retry, foreman_wakes_per_day, default_reviewer, rounds.fix, rounds.review, rounds.restart, review.tier, review.effort, review.family, a caps.<layer>, a cost.<harness>/<model>, a tier.<harness>/<model>, a tools.claude/<model> or an env.<harness>.<VAR>", path, n+1, key)
		}
		if err != nil {
			return FileConfig{}, fmt.Errorf("config %s: line %d: %s: %v", path, n+1, key, err)
		}
	}
	return cfg, nil
}

// setRounds sets one rounds.fix, rounds.review or rounds.restart key to a
// non-negative integer.
func setRounds(r *RoundsConfig, key, value string) error {
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return fmt.Errorf("want a non-negative integer, got %q", value)
	}
	switch key {
	case "rounds.fix":
		r.Fix = &n
	case "rounds.review":
		r.Review = &n
	case "rounds.restart":
		r.Restart = &n
	default:
		return fmt.Errorf("unknown rounds key; want rounds.fix, rounds.review or rounds.restart")
	}
	return nil
}

// setTools sets one tools.<harness>/<model> key: the claude worker scope
// default of that pair. Any other harness is refused with its own
// sandbox model's description.
func setTools(cfg *FileConfig, key, value string) error {
	rest, _ := strings.CutPrefix(key, "tools.")
	harness, model, ok := strings.Cut(rest, "/")
	if !ok || harness == "" || model == "" {
		return fmt.Errorf("want tools.<harness>/<model>")
	}
	if harness != "claude" {
		return fmt.Errorf("%s", ToolsRefusal(harness))
	}
	tools, err := ParseTools(value)
	if err != nil {
		return err
	}
	if cfg.Tools == nil {
		cfg.Tools = map[string][]string{}
	}
	cfg.Tools[harness+"/"+model] = tools
	return nil
}

// setCap sets one caps, cost or tier key of §8.1 on l, mirroring the YAML
// example there in the file's flat `key = value` shape: caps.global,
// caps.plan, caps.runner, caps.account.<label>, caps.model.<model> and
// cost.<harness>/<model> or tier.<harness>/<model>. Tier values are checked
// against the tier vocabulary; other values are non-negative integers. A key
// the file never names leaves the layer uncapped — or, for cost, the
// weight at its default of 1, or the tier from the model-name table.
func setCap(l *caps.Limits, key, value string) error {
	if rest, ok := strings.CutPrefix(key, "tier."); ok {
		harness, model, ok := strings.Cut(rest, "/")
		if !ok || harness == "" || model == "" {
			return fmt.Errorf("want tier.<harness>/<model>")
		}
		if value == "" {
			return fmt.Errorf("want one of %s", strings.Join(adapter.Tiers, ", "))
		}
		if err := adapter.ValidateTier(value); err != nil {
			return err
		}
		if l.Tier == nil {
			l.Tier = map[string]string{}
		}
		l.Tier[harness+"/"+model] = value
		return nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return fmt.Errorf("want a non-negative integer, got %q", value)
	}
	if rest, ok := strings.CutPrefix(key, "cost."); ok {
		harness, model, ok := strings.Cut(rest, "/")
		if !ok || harness == "" || model == "" {
			return fmt.Errorf("want cost.<harness>/<model>")
		}
		if l.Cost == nil {
			l.Cost = map[string]int{}
		}
		l.Cost[harness+"/"+model] = n
		return nil
	}
	rest, _ := strings.CutPrefix(key, "caps.")
	switch {
	case rest == "global":
		l.Global = &n
	case rest == "plan":
		l.Plan = &n
	case rest == "runner":
		l.Runner = &n
	default:
		if label, ok := strings.CutPrefix(rest, "account."); ok && label != "" {
			if l.Account == nil {
				l.Account = map[string]int{}
			}
			l.Account[label] = n
			return nil
		}
		if model, ok := strings.CutPrefix(rest, "model."); ok && model != "" {
			if l.Model == nil {
				l.Model = map[string]int{}
			}
			l.Model[model] = n
			return nil
		}
		return fmt.Errorf("unknown caps layer %q; want global, plan, runner, account.<label> or model.<model>", rest)
	}
	return nil
}
