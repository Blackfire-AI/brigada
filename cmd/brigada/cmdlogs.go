package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/adapter/codex"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/adapter/kimi"
	"github.com/Blackfire-AI/brigada/internal/adapter/opencode"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// logsClient is what logs asks of the engine: a task's latest job, a
// job's artifact ids and status, the artifact blobs, and the status the
// follow polls.
type logsClient interface {
	Task(ctx context.Context, id int64) (engine.TaskInfo, error)
	Job(ctx context.Context, id int64) (engine.JobInfo, error)
	Artifact(ctx context.Context, id int64) ([]byte, error)
	Status(ctx context.Context) (api.Status, error)
}

// followInterval is how often --follow asks the engine. A var so the
// tests can shorten it.
var followInterval = 2 * time.Second

// followTail is how many lines of the transcript --follow prints once
// the job has ended, when --tail does not say.
const followTail = 20

// logLineLimit bounds one rendered line: a message can run to pages.
const logLineLimit = 300

// runLogs is `brigada logs <job-id> [--check] [--tail N] [--follow]`: the
// job's transcript as readable lines, or its check log. `--task N`
// names the task's latest job instead of a job id.
func runLogs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("logs")
	common := addCommon(fs)
	var (
		check   bool
		follow  bool
		tail    int
		task    int64
		noStart bool
	)
	fs.BoolVar(&check, "check", false, "print the job's check log instead of its transcript")
	fs.BoolVar(&follow, "follow", false, "poll a running job's latest progress until it ends, then print the transcript tail")
	fs.IntVar(&tail, "tail", 0, "print only the last N lines")
	fs.Int64Var(&task, "task", 0, "name a task's latest job instead of a job id")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	const usage = "usage: brigada logs <job-id> [--check] [--tail N] [--follow]  |  brigada logs --task N [...]"
	rest, code := parseWithPositional(common, stderr, args)
	if code != 0 {
		return code
	}
	var job int64
	switch {
	case task != 0 && len(rest) == 0:
	case task == 0 && len(rest) == 1:
		var ok bool
		if job, ok = parseID(stderr, "job id", rest[0]); !ok {
			return 2
		}
	default:
		return usagef(stderr, "%s", usage)
	}
	if task < 0 || tail < 0 {
		return usagef(stderr, "--task and --tail must be non-negative")
	}
	if follow && check {
		return usagef(stderr, "--follow follows the transcript; it does not combine with --check")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	if task != 0 {
		t, err := client.Task(ctx, task)
		if err != nil {
			return fail(stderr, notFoundAs(err, "task %d: no such task", task))
		}
		if t.LatestJobID == 0 {
			return fail(stderr, fmt.Errorf("task %d has no job yet", task))
		}
		job = t.LatestJobID
	}
	return printLogs(ctx, client, job, check, follow, tail, stdout, stderr)
}

// notFoundAs words a 404 as format, and leaves any other error alone.
func notFoundAs(err error, format string, args ...any) error {
	if isNotFound(err) {
		return fmt.Errorf(format, args...)
	}
	return err
}

// printLogs prints one job's record: the check log verbatim, or the
// transcript rendered, in both cases cut to the last tail lines when
// tail is set. With follow it first shows the running job's progress.
func printLogs(ctx context.Context, client logsClient, jobID int64, check, follow bool, tail int, stdout, stderr io.Writer) int {
	if follow {
		if code := followJob(ctx, client, jobID, stdout, stderr); code != 0 {
			return code
		}
		if ctx.Err() != nil {
			return 0
		}
		if tail == 0 {
			tail = followTail
		}
	}
	job, err := client.Job(ctx, jobID)
	if err != nil {
		return fail(stderr, notFoundAs(err, "job %d: no such job", jobID))
	}
	artifact, what := job.TranscriptID, "transcript"
	if check {
		artifact, what = job.CheckLogID, "check log"
	}
	if artifact == 0 {
		return fail(stderr, fmt.Errorf("job %d has no %s (status %s)", jobID, what, job.Status))
	}
	data, err := client.Artifact(ctx, artifact)
	if err != nil {
		return fail(stderr, err)
	}
	var lines []string
	if check {
		lines = splitLines(string(data))
	} else {
		lines = renderTranscript(data)
	}
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	for _, l := range lines {
		fmt.Fprintln(stdout, l)
	}
	return 0
}

// followJob polls the status for the job's latest progress event every
// followInterval and prints each new one, until the job is no longer
// live. A job still queued (not yet on a runner) is waited for.
func followJob(ctx context.Context, client logsClient, jobID int64, stdout, stderr io.Writer) int {
	last := ""
	for {
		st, err := client.Status(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			return fail(stderr, err)
		}
		running := false
		for _, j := range st.Running {
			if j.ID != jobID {
				continue
			}
			running = true
			if j.LastEvent != "" && j.LastEvent != last {
				last = j.LastEvent
				fmt.Fprintln(stdout, last)
			}
		}
		if !running {
			job, err := client.Job(ctx, jobID)
			if err != nil {
				if ctx.Err() != nil {
					return 0
				}
				return fail(stderr, notFoundAs(err, "job %d: no such job", jobID))
			}
			switch job.Status {
			case "queued", "leased", "running":
			default:
				fmt.Fprintf(stdout, "job %d: %s\n", jobID, job.Status)
				return 0
			}
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(followInterval):
		}
	}
}

// splitLines cuts text into lines, without the empty one a closing
// newline leaves.
func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// transcriptAdapters are the parsers a transcript can have been written
// by; the transcript itself does not say which one ran.
func transcriptAdapters() []adapter.Adapter {
	return []adapter.Adapter{&claude.Adapter{}, &kimi.Adapter{}, &codex.Adapter{}, &opencode.Adapter{}, &fake.Adapter{}}
}

// renderTranscript turns a transcript artifact — the harness's own event
// stream, one line each, stderr lines tagged — into readable lines:
// messages, tool starts and ends with the tool, errors. The harness is
// the adapter that reads most of the lines as progress; lines it does
// not track (banners, session ids) are left out, the runner's own
// compaction markers stay. A transcript nothing could read is printed as
// it lies.
func renderTranscript(data []byte) []string {
	raw := splitLines(string(data))
	var best adapter.Adapter
	bestN := 0
	for _, ad := range transcriptAdapters() {
		n := 0
		for _, l := range raw {
			if _, isErr := strings.CutPrefix(l, "stderr: "); isErr {
				continue
			}
			if ev, ok := ad.ParseEvent([]byte(l)); ok && ev.IsProgress() {
				n++
			}
		}
		if n > bestN {
			best, bestN = ad, n
		}
	}
	if best == nil {
		best = transcriptAdapters()[0]
	}
	var out []string
	for _, l := range raw {
		if strings.HasPrefix(l, "…") {
			out = append(out, l) // the runner's marker for a dropped middle
			continue
		}
		var ev adapter.Event
		var ok bool
		if text, isErr := strings.CutPrefix(l, "stderr: "); isErr {
			ev, ok = best.ParseErrEvent([]byte(text))
		} else {
			ev, ok = best.ParseEvent([]byte(l))
		}
		if !ok {
			continue
		}
		switch ev.Kind {
		case adapter.EvMessage, adapter.EvToolStart, adapter.EvToolEnd:
			out = append(out, logLine(string(ev.Kind), ev.Text))
		case adapter.EvError:
			kind := "error"
			if ev.ErrorKind != "" {
				kind += " (" + string(ev.ErrorKind) + ")"
			}
			out = append(out, logLine(kind, ev.Text))
		}
	}
	if len(out) == 0 {
		return raw
	}
	return out
}

// logLine is one rendered line: the kind, then the text flattened to a
// single line and cut at logLineLimit.
func logLine(kind, text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > logLineLimit {
		text = string(r[:logLineLimit]) + "…"
	}
	if text == "" {
		return kind
	}
	return kind + " " + text
}
