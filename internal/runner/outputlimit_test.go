package runner

import (
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/prompt"
)

// outputLimitScript is a fake worker whose last model step was cut off
// at its output limit, then ran tail.
func outputLimitScript(tail string) string {
	return `cat <<'EOF'
{"type":"usage","output_limit":true,"usage":{"input_tokens":10,"output_tokens":49,"reasoning_tokens":31951}}
EOF
` + tail
}

// TestRunnerOutputLimitWithoutResultIsAnError covers a run cut off at its
// output limit that left no result block: it completes with the
// output_limit kind and the numbers in its text.
func TestRunnerOutputLimitWithoutResultIsAnError(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	markerDir(t)

	script := writeWorkerScript(t, outputLimitScript(fakeResultScript("I was thinking about it.")))
	p := e.startRunner(script, 100*time.Millisecond)
	defer p.stop(t)
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))
	failed := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "failed" })

	if got, want := e.errorResult(failed), "output limit reached: 32,000 tokens, 31,951 of them reasoning"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
	if got := e.strOf("SELECT COALESCE(error_kind, '') FROM jobs WHERE id = ?", job); got != "output_limit" {
		t.Errorf("error kind = %q, want output_limit", got)
	}
}

// TestRunnerOutputLimitWithResultStands covers the same cut-off when the
// run still ended with a readable result block: no error is recorded.
func TestRunnerOutputLimitWithResultStands(t *testing.T) {
	t.Parallel()
	remote := newRemote(t)
	e := newTestEnv(t, engine.Config{})
	e.seedProject(remote)
	task := e.seedTask("", "true")
	markerDir(t)

	block := "```json\n{\"status\":\"needs_help\",\"question\":\"which file?\"}\n```"
	script := writeWorkerScript(t, outputLimitScript(fakeResultScript(block)))
	p := e.startRunner(script, 100*time.Millisecond)
	defer p.stop(t)
	job := e.submitWork(task, prompt.ComposeWorkerPrompt("Add work.txt.", "", ""))
	done := e.waitJob(job, func(j *engine.JobInfo) bool { return j.Status == "done" || j.Status == "failed" })

	if got := e.errorResult(done); got != "" {
		t.Errorf("error = %q, want none: the result block stands", got)
	}
	if got := e.strOf("SELECT COALESCE(error_kind, '') FROM jobs WHERE id = ?", job); got != "" {
		t.Errorf("error kind = %q, want none", got)
	}
}

func TestGroupThousands(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 32000: "32,000", 1234567: "1,234,567"} {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", in, got, want)
		}
	}
}
