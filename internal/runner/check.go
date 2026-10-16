package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"time"
)

// checkTailBytes is how much of the check's output the result keeps
// (§5.5): enough to name the failure, never the whole log — the log
// itself is an artifact.
const checkTailBytes = 4 << 10

// checkKillGrace is how long a timed-out check's Wait keeps collecting
// output after its process group was killed (exec.Cmd.WaitDelay): the
// kill closes the pipes in practice, and the grace bounds the case a
// straggler holds one open anyway.
const checkKillGrace = 3 * time.Second

// defaultCheckTimeout bounds one check run. A task may carry its own
// through the runner's config; the default keeps a hung check from
// holding the lease forever.
const defaultCheckTimeout = 10 * time.Minute

// CheckResult is what the runner reports about a check it ran (§5.5):
// the exit code, whether the timeout cut it short, and the last of its
// output. A non-zero exit is a failed check whatever it printed.
type CheckResult struct {
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
	Output   string `json:"output_tail"`
	// Log is the whole captured output, for the check log artifact (§9);
	// the tail above is what the result carries.
	Log string `json:"-"`
}

// Failed reports whether the check failed: a non-zero exit or a
// timeout. The kill a timeout performs usually shows up as a non-zero
// exit too, but the timeout is named on its own so a killed check can
// never read as a pass. A check that did not run (no command) has
// neither.
func (c CheckResult) Failed() bool {
	return c.ExitCode != 0 || c.TimedOut
}

// JSON renders the check result for the engine's check_result column.
func (c CheckResult) JSON() string {
	b, err := json.Marshal(c)
	if err != nil {
		return `{"exit_code":-1,"output_tail":"unserializable check result"}`
	}
	return string(b)
}

// runCheck runs the task's check command in dir (§5.5): the runner, not
// the worker, runs it, so the outcome record stays honest. The command
// runs through sh in its own process group; on timeout the whole group
// is killed — killing only sh would leave the check's grandchildren
// (a build, a test binary) holding the output pipe and the run hanging
// — and the last checkTailBytes of its combined output are kept.
func runCheck(ctx context.Context, dir, command string, timeout time.Duration, env ...string) CheckResult {
	if command == "" {
		return CheckResult{}
	}
	if timeout <= 0 {
		timeout = defaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	// The extra checkouts' variables (§5.8) join the inherited environment.
	cmd.Env = withEnv(nil, env)
	cmd.SysProcAttr = processGroup()
	// The context's cancel kills the group, not just sh; WaitDelay
	// bounds the wait for the pipes the killed tree can no longer
	// close, so the timeout is enforced whatever the check spawned.
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = checkKillGrace
	out, err := cmd.CombinedOutput()
	res := CheckResult{
		ExitCode: exitCode(err),
		Output:   tailOf(string(out), checkTailBytes),
		Log:      string(out),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	return res
}

// exitCode reads a command's exit status; -1 when it died to a signal
// or never ran, which a failed check is.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
