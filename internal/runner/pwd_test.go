package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// pwdProbe wraps the fake adapter so that the command the runner starts
// first re-executes this test binary as a probe: it records the
// environment it was started with and whether PWD names its working
// directory, then execs the real script. Unlike a shell script, it sees
// the raw PWD the runner handed over — a shell resets a PWD that does
// not match its directory before a script can look at it.
type pwdProbe struct{ *fake.Adapter }

func (p pwdProbe) Command(job adapter.Job, workdir, resume string) (*exec.Cmd, error) {
	cmd, err := p.Adapter.Command(job, workdir, resume)
	if err != nil {
		return nil, err
	}
	script := cmd.Path
	cmd.Path = os.Args[0]
	cmd.Args = []string{os.Args[0], "-test.run=^TestPWDProbeHelper$"}
	cmd.Env = append(cmd.Env, "BRIGADA_PWD_PROBE_SCRIPT="+script)
	return cmd, nil
}

// TestPWDProbeHelper is the probe's body, run as a child process; in a
// normal test run it does nothing.
func TestPWDProbeHelper(t *testing.T) {
	script := os.Getenv("BRIGADA_PWD_PROBE_SCRIPT")
	if script == "" {
		return
	}
	env := os.Environ()
	here, errHere := os.Stat(".")
	same := false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PWD="); ok {
			if there, err := os.Stat(v); err == nil && errHere == nil {
				same = os.SameFile(here, there)
			}
		}
	}
	out := append(env, fmt.Sprintf("PROBE_PWD_IS_CWD=%t", same))
	dst := filepath.Join(os.Getenv("BRIGADA_TEST_MARKERS"), fmt.Sprintf("probe-%d", os.Getpid()))
	if err := os.WriteFile(dst, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		os.Exit(3)
	}
	_ = syscall.Exec(script, []string{script}, env)
	os.Exit(4)
}

// TestRunnerSetsPWDToTheWorktree: the runner's own PWD points elsewhere
// (the state directory, in life), yet the work job's CLI and the review
// job's CLI both start with PWD naming their own working directory,
// once, and no OLDPWD.
func TestRunnerSetsPWDToTheWorktree(t *testing.T) {
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	markers := markerDir(t)
	t.Setenv("PWD", t.TempDir())
	t.Setenv("OLDPWD", t.TempDir())
	task := e.seedTask("", "true")
	work := e.submitWork(task, prompt.ComposeWorkerPrompt("the task spec", "", ""))

	t.Setenv("BRIGADA_TEST_BRANCH", taskBranch(task, 1))
	p := e.startRunner(writeWorkerScript(t, reviewScript(reviewApproveResult)), 100*time.Millisecond, withReviewerAgent,
		func(c *Config) {
			for i, ad := range c.Adapters {
				c.Adapters[i] = pwdProbe{ad.(*fake.Adapter)}
			}
		})
	defer p.stop(t)

	e.waitJob(work, func(j *engine.JobInfo) bool { return j.Status == "done" })
	e.waitTaskState(task, "done")

	probes, _ := filepath.Glob(filepath.Join(markers, "probe-*"))
	if len(probes) != 2 {
		t.Fatalf("%d probes ran, want the work job's and the review's", len(probes))
	}
	for _, f := range probes {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var pwds, oldpwds int
		for _, kv := range strings.Split(string(data), "\n") {
			switch {
			case strings.HasPrefix(kv, "PWD="):
				pwds++
			case strings.HasPrefix(kv, "OLDPWD="):
				oldpwds++
			case kv == "PROBE_PWD_IS_CWD=false":
				t.Errorf("%s: PWD is not the command's working directory", filepath.Base(f))
			}
		}
		if pwds != 1 || oldpwds != 0 {
			t.Errorf("%s: %d PWD and %d OLDPWD entries, want one and none", filepath.Base(f), pwds, oldpwds)
		}
	}
	e.assertNoWorktrees()
}

func TestWithPWD(t *testing.T) {
	got := withPWD([]string{"A=1", "PWD=/old", "OLDPWD=/older", "PWD=/dup"}, "/work/tree")
	want := []string{"A=1", "PWD=/work/tree"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("withPWD = %v, want %v", got, want)
	}
}
