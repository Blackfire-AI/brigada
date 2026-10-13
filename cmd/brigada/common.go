package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
)

// newFlagSet builds a command's flag set with its output muted: usage
// errors are printed by the command, with its name and exit code.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// commonFlags are the flags every command shares (§2): where the state
// lives and which config file tunes it. Resolution order is defaults,
// then the config file, then the flag.
type commonFlags struct {
	fs       *flag.FlagSet
	stateDir string
	config   string
}

// addCommon registers the common flags on fs and keeps them by its side.
func addCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{fs: fs}
	fs.StringVar(&c.stateDir, "state-dir", "", "state directory (default: $XDG_STATE_HOME/brigada, else ~/.local/state/brigada)")
	fs.StringVar(&c.config, "config", "", "config file (default: $XDG_CONFIG_HOME/brigada/config, else ~/.config/brigada/config)")
	return c
}

// parse parses args, reporting a flag error as exit code 2.
func (c *commonFlags) parse(stderr io.Writer, args []string) int {
	if err := c.fs.Parse(args); err != nil {
		return usagef(stderr, "%v", err)
	}
	return 0
}

// runtime is one command's resolved context: the state directory, the
// engine's paths inside it, and the config file that tuned them.
type runtime struct {
	stateDir    string
	paths       lifecycle.Paths
	configPath  string // the config file consulted; "" when none was
	explicitCfg bool   // the config file was named explicitly
	file        lifecycle.FileConfig
}

// resolveRuntime folds defaults, the config file and the flags into one
// runtime. A config file named explicitly must exist; the default one is
// consulted only when it does.
func resolveRuntime(c *commonFlags, stderr io.Writer) (*runtime, int) {
	path := c.config
	explicit := path != ""
	if path == "" {
		path = lifecycle.DefaultConfigPath()
	}
	file, err := lifecycle.LoadConfigFile(path)
	if err != nil {
		return nil, fail(stderr, err)
	}
	stateDir := c.stateDir
	if stateDir == "" {
		stateDir = file.StateDir
	}
	if stateDir == "" {
		stateDir = lifecycle.DefaultStateDir()
	}
	rt := &runtime{
		stateDir:    stateDir,
		paths:       lifecycle.PathsOf(stateDir),
		configPath:  path,
		explicitCfg: explicit,
		file:        file,
	}
	if err := lifecycle.CheckSocketPath(rt.paths.Socket); err != nil {
		return nil, fail(stderr, err)
	}
	return rt, 0
}

// configForStart is the config path to hand an engine started on demand
// (§13): the file the client named, so the engine reads what the client
// read; a default file the engine finds on its own.
func (rt *runtime) configForStart() string {
	if rt.explicitCfg {
		return rt.configPath
	}
	return ""
}

// ensureEngine makes sure an engine answers on the runtime's socket
// (§13): started on demand unless noStart refuses. Client commands and
// the runner share it.
func ensureEngine(ctx context.Context, rt *runtime, noStart bool, stderr io.Writer) int {
	if noStart {
		if !lifecycle.EngineRunning(ctx, rt.paths.Socket) {
			return fail(stderr, fmt.Errorf("no engine is answering on %s; --no-start is set, so it was not started", rt.paths.Socket))
		}
		return 0
	}
	if err := lifecycle.EnsureEngine(ctx, lifecycle.StartOptions{
		Socket:     rt.paths.Socket,
		StateDir:   rt.stateDir,
		ConfigPath: rt.configForStart(),
	}); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// connect ensures an engine answers and returns a client speaking with
// the given role's token (§13).
func connect(ctx context.Context, rt *runtime, role string, noStart bool, stderr io.Writer) (*api.Client, int) {
	if code := ensureEngine(ctx, rt, noStart, stderr); code != 0 {
		return nil, code
	}
	_, secret, err := lifecycle.TokenForRole(rt.paths.Tokens, role)
	if err != nil {
		return nil, fail(stderr, err)
	}
	return api.NewClient(rt.paths.Socket, secret), 0
}

// formatUptime renders an uptime: whole seconds, nothing sub-second, so
// a status taken twice in one second reads the same twice.
func formatUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// parseStartedAt reads the engine's started_at into a time, for the
// uptime; a zero time means "not recorded".
func parseStartedAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// runnerStateDir is where the runner role keeps its own workspace —
// mirrors, worktrees, records — inside the shared state directory.
func runnerStateDir(rt *runtime) string {
	return filepath.Join(rt.stateDir, "runner")
}
