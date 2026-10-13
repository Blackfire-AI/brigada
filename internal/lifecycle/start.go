package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// startWait is how long a client starting the engine on demand waits for
// it to answer /v1/health before giving up (§13).
const startWait = 5 * time.Second

// startPoll is how often the on-demand starter re-probes health.
const startPoll = 100 * time.Millisecond

// StartOptions is what starting the engine on demand needs (§13): where
// it answers, where its state lives, and — for the tests — which binary
// to start.
type StartOptions struct {
	Socket   string
	StateDir string
	// ConfigPath, when set, is passed to the engine so it reads the same
	// config file the client did.
	ConfigPath string
	// Binary is the brigada to start; empty means this executable.
	Binary string
	// Wait overrides the 5 s health wait; zero keeps it.
	Wait time.Duration
	// Log is where the starter's messages go; nil uses slog's default.
	Log *slog.Logger
}

// EnsureEngine makes sure an engine answers on the socket (§13): a probe
// first — an engine already there is never disturbed — then, when
// nothing answers, `brigada engine` started in the background, detached
// in its own session and logging into the state directory, and a bounded
// wait for its health.
func EnsureEngine(ctx context.Context, o StartOptions) error {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	if err := CheckSocketPath(o.Socket); err != nil {
		return err
	}
	probe := api.NewClient(o.Socket, "")
	if err := probe.HealthProbe(ctx); err == nil {
		return nil
	}
	if err := startEngine(ctx, o, log); err != nil {
		return err
	}
	wait := o.Wait
	if wait <= 0 {
		wait = startWait
	}
	deadline := time.Now().Add(wait)
	for {
		if err := probe.HealthProbe(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the engine did not answer on %s within %s; its log is %s",
				o.Socket, wait, filepath.Join(o.StateDir, "engine.log"))
		}
		time.Sleep(startPoll)
	}
}

// EngineRunning probes whether an engine answers on the socket: the
// start-on-demand question without the start.
func EngineRunning(ctx context.Context, socket string) bool {
	return api.NewClient(socket, "").HealthProbe(ctx) == nil
}

// startEngine spawns `brigada engine` detached (§13): its own session,
// blind on stdin, stdout and stderr into the state directory's log file.
// The starter does not wait for it — the health poll that follows is the
// only contract — so a second engine spawned into a race with a first
// simply takes the lock refusal and exits into the same log.
func startEngine(ctx context.Context, o StartOptions, log *slog.Logger) error {
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return fmt.Errorf("create the state directory %s: %w", o.StateDir, err)
	}
	binary := o.Binary
	if binary == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("find the brigada binary to start: %w", err)
		}
		binary = self
	}
	args := []string{"engine", "--state-dir", o.StateDir}
	if o.ConfigPath != "" {
		args = append(args, "--config", o.ConfigPath)
	}
	logFile, err := os.OpenFile(filepath.Join(o.StateDir, "engine.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the engine log: %w", err)
	}
	defer logFile.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devNull.Close()
	log.InfoContext(ctx, "starting the engine", "binary", binary, "state_dir", o.StateDir)
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s engine: %w", binary, err)
	}
	// Detached: the engine is nobody's child to wait on. Release leaves
	// it to its session; its life is the lock's business now (§13).
	return cmd.Process.Release()
}
