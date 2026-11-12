//go:build qwenlive

package qwencode

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// TestLive runs the real, logged-in qwen CLI: two free probes and exactly one
// metered headless run. Run it by hand:
//
//	go test -tags qwenlive -count=1 -run TestLive -v ./internal/adapter/qwencode
func TestLive(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"--help"}} {
		out, err := exec.Command("qwen", args...).CombinedOutput()
		t.Logf("qwen %s (err=%v):\n%s", args[0], err, out)
		if err != nil {
			t.Fatalf("qwen %s: %v", args[0], err)
		}
	}

	repo := t.TempDir()
	for _, g := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "init"}} {
		c := exec.Command("git", g...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", g, err, out)
		}
	}

	job := adapter.Job{Prompt: "Create a file hello.txt containing exactly: hi\nThen reply with the single word: done"}
	cmd, err := (&Adapter{}).Command(job, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Logf("exit err = %v", err)
	case <-time.After(5 * time.Minute):
		_ = cmd.Process.Kill()
		t.Fatal("qwen run timed out")
	}
	t.Logf("RAW STDOUT:\n%s", stdout.String())
	t.Logf("RAW STDERR:\n%s", stderr.String())

	a := &Adapter{}
	var events []adapter.Event
	var sessionID string
	var usage *adapter.Usage
	collect := func(ev adapter.Event, ok bool) {
		if !ok {
			return
		}
		events = append(events, ev)
		if ev.Kind == adapter.EvSession && ev.SessionID != "" {
			sessionID = ev.SessionID
		}
		if ev.Usage != nil {
			if usage == nil {
				usage = &adapter.Usage{}
			}
			usage.Add(ev.Usage)
		}
	}
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		collect(a.ParseEvent(sc.Bytes()))
	}
	sc = bufio.NewScanner(&stderr)
	for sc.Scan() {
		collect(a.ParseErrEvent(sc.Bytes()))
	}
	for _, ev := range events {
		t.Logf("event: %+v", ev)
	}

	if sessionID == "" {
		t.Error("no session id")
	}
	if strings.TrimSpace(a.ResultText(events)) == "" {
		t.Error("empty ResultText")
	}
	if usage == nil || usage.InputTokens == nil || usage.OutputTokens == nil {
		t.Errorf("usage lacks input/output counts: %+v", usage)
	} else {
		if *usage.InputTokens <= 0 || *usage.OutputTokens <= 0 {
			t.Errorf("usage counts are zero: in %d out %d", *usage.InputTokens, *usage.OutputTokens)
		}
		if usage.CachedInputTokens != nil && *usage.CachedInputTokens > *usage.InputTokens {
			t.Errorf("cached %d exceeds input %d", *usage.CachedInputTokens, *usage.InputTokens)
		}
	}
	b, err := os.ReadFile(filepath.Join(repo, "hello.txt"))
	if err != nil {
		t.Errorf("hello.txt not written (auto-approve flag?): %v", err)
	} else if !strings.Contains(string(b), "hi") {
		t.Errorf("hello.txt = %q", b)
	}
}
