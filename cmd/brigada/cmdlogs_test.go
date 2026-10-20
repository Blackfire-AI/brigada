package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

type stubLogs struct {
	tasks     map[int64]engine.TaskInfo
	jobs      map[int64]engine.JobInfo
	artifacts map[int64]string
	statuses  []api.Status // one per Status call, the last repeating
	calls     int
}

func (s *stubLogs) Task(_ context.Context, id int64) (engine.TaskInfo, error) {
	return s.tasks[id], nil
}

func (s *stubLogs) Job(_ context.Context, id int64) (engine.JobInfo, error) {
	j, ok := s.jobs[id]
	if !ok {
		return engine.JobInfo{}, fmt.Errorf("no job %d", id)
	}
	return j, nil
}

func (s *stubLogs) Artifact(_ context.Context, id int64) ([]byte, error) {
	return []byte(s.artifacts[id]), nil
}

func (s *stubLogs) Status(context.Context) (api.Status, error) {
	i := s.calls
	if i >= len(s.statuses) {
		i = len(s.statuses) - 1
	}
	s.calls++
	return s.statuses[i], nil
}

const seededTranscript = `{"type":"system","subtype":"init","session_id":"sess-1","model":"sonnet","tools":["Read","Edit"]}
claude-code 2.0.1 — starting headless session

{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the task,\nthen adding the file."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","id":"toolu-1","input":{"file_path":"README.md"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu-1","content":"# hello\n","is_error":false}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Added work.txt."}]}}
`

const renderedTranscript = "message Reading the task, then adding the file.\ntool_start Read\ntool_end toolu-1\nmessage Added work.txt.\n"

func newStubLogs() *stubLogs {
	return &stubLogs{
		tasks: map[int64]engine.TaskInfo{3: {ID: 3, LatestJobID: 12}},
		jobs: map[int64]engine.JobInfo{
			11: {ID: 11, Status: "done", TranscriptID: 100},
			12: {ID: 12, Status: "done", TranscriptID: 101, CheckLogID: 102},
		},
		artifacts: map[int64]string{
			100: seededTranscript,
			101: `{"type":"assistant","message":{"content":[{"type":"text","text":"the later job"}]}}` + "\n",
			102: "ok  \tpkg\t0.1s\nFAIL\tother\n",
		},
	}
}

func TestLogsRendersTheTranscript(t *testing.T) {
	var out, errb bytes.Buffer
	if code := printLogs(context.Background(), newStubLogs(), 11, false, false, 0, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if out.String() != renderedTranscript {
		t.Errorf("transcript rendered as\n%q\nwant\n%q", out.String(), renderedTranscript)
	}

	out.Reset()
	if code := printLogs(context.Background(), newStubLogs(), 11, false, false, 2, &out, &errb); code != 0 {
		t.Fatalf("--tail exit %d, stderr %q", code, errb.String())
	}
	if want := "tool_end toolu-1\nmessage Added work.txt.\n"; out.String() != want {
		t.Errorf("--tail 2 = %q, want %q", out.String(), want)
	}
}

func TestLogsCheckLog(t *testing.T) {
	var out, errb bytes.Buffer
	if code := printLogs(context.Background(), newStubLogs(), 12, true, false, 0, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if want := "ok  \tpkg\t0.1s\nFAIL\tother\n"; out.String() != want {
		t.Errorf("check log = %q, want %q", out.String(), want)
	}
	out.Reset()
	printLogs(context.Background(), newStubLogs(), 12, true, false, 1, &out, &errb)
	if out.String() != "FAIL\tother\n" {
		t.Errorf("--check --tail 1 = %q", out.String())
	}
	// A job with no check log says so.
	errb.Reset()
	if code := printLogs(context.Background(), newStubLogs(), 11, true, false, 0, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no check log") {
		t.Errorf("exit %d, stderr %q, want the no-check-log answer", code, errb.String())
	}
}

func TestLogsRendersErrorsAndUnreadableTranscripts(t *testing.T) {
	lines := renderTranscript([]byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}\n"))
	if len(lines) != 1 || lines[0] != "message hi" {
		t.Errorf("rendered %q, want the one message", lines)
	}
	raw := renderTranscript([]byte("just some prose\nmore prose\n"))
	if strings.Join(raw, "|") != "just some prose|more prose" {
		t.Errorf("an unreadable transcript rendered %q, want it as it lies", raw)
	}
}

func TestLogsRendersDshTranscript(t *testing.T) {
	transcript := `{"type":"session","sessionId":"session-1","cwd":"/work"}
{"type":"text","text":"checking"}
{"type":"tool_call","callId":"c1","tool":"bash","input":{"command":"ls"}}
{"type":"tool_result","callId":"c1","status":"completed","result":"ok"}
stderr: dsh: AUTH: bad key
{"type":"final","text":"done"}
`
	got := strings.Join(renderTranscript([]byte(transcript)), "|")
	want := "message checking|tool_start bash|tool_end c1|error (auth) bad key|message done"
	if got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

func TestLogsTaskPicksTheLatestJob(t *testing.T) {
	stub := newStubLogs()
	// The flag path resolves the task's latest job before printing.
	job := stub.tasks[3].LatestJobID
	var out, errb bytes.Buffer
	if code := printLogs(context.Background(), stub, job, false, false, 0, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if out.String() != "message the later job\n" {
		t.Errorf("logs --task 3 = %q, want job 12's transcript", out.String())
	}
}

func TestLogsUsage(t *testing.T) {
	for _, args := range [][]string{{"logs"}, {"logs", "1", "--task", "2"}, {"logs", "1", "--follow", "--check"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr %q", args, code, errb.String())
		}
	}
}

func TestLogsFollowPrintsProgressUntilTheJobEnds(t *testing.T) {
	old := followInterval
	followInterval = 0
	t.Cleanup(func() { followInterval = old })

	running := func(ev string) api.Status {
		return api.Status{Running: []engine.StatusJob{{ID: 11, LastEvent: ev}}}
	}
	stub := newStubLogs()
	stub.statuses = []api.Status{running("tool_start Read"), running("tool_start Read"), running("message done soon"), {}}
	var out, errb bytes.Buffer
	if code := printLogs(context.Background(), stub, 11, false, true, 0, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	want := "tool_start Read\nmessage done soon\njob 11: done\n" + renderedTranscript
	if out.String() != want {
		t.Errorf("follow printed\n%q\nwant\n%q", out.String(), want)
	}
}
