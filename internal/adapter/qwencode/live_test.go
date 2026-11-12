//go:build qwenlive

package qwencode

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// TestLive drives the real, signed-in qwen CLI once: one headless model
// call. Run it by hand, never in the default check:
//
//	go test -tags qwenlive -count=1 -run TestLive -v ./internal/adapter/qwencode
func TestLive(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"--help"}} {
		out, err := exec.Command("qwen", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("qwen %v: %v\n%s", args, err, out)
		}
		t.Logf("qwen %v:\n%s", args, out)
	}

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.invalid"}, {"config", "user.name", "t"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	job := adapter.Job{Prompt: "Create a file named hello.txt containing exactly: hi\nThen reply with the single word: done"}
	cmd, err := (&Adapter{}).Command(job, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	cmd.Dir, cmd.Env = repo, workerEnv(os.Environ())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	a := &Adapter{}
	var (
		mu     sync.Mutex
		events []adapter.Event
		wg     sync.WaitGroup
	)
	scan := func(name string, r interface{ Read([]byte) (int, error) }, parse func([]byte) (adapter.Event, bool)) {
		defer wg.Done()
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for s.Scan() {
			line := bytes.Clone(s.Bytes())
			mu.Lock()
			t.Logf("%s: %s", name, line)
			if ev, ok := parse(line); ok {
				events = append(events, ev)
			}
			mu.Unlock()
		}
	}
	wg.Add(2)
	go scan("stdout", stdout, a.ParseEvent)
	go scan("stderr", stderr, a.ParseErrEvent)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		t.Errorf("qwen exited: %v", err)
	}

	var session string
	var in, out int64
	for _, ev := range events {
		if ev.Kind == adapter.EvError {
			t.Errorf("error event: %+v", ev)
		}
		if ev.SessionID != "" {
			session = ev.SessionID
		}
		if ev.Usage != nil {
			if ev.Usage.InputTokens != nil {
				in += *ev.Usage.InputTokens
			}
			if ev.Usage.OutputTokens != nil {
				out += *ev.Usage.OutputTokens
			}
			if c := ev.Usage.CachedInputTokens; c != nil && ev.Usage.InputTokens != nil && *c > *ev.Usage.InputTokens {
				t.Errorf("cached %d exceeds input %d", *c, *ev.Usage.InputTokens)
			}
		}
	}
	if session == "" {
		t.Error("no session id")
	}
	if strings.TrimSpace(a.ResultText(events)) == "" {
		t.Error("empty ResultText")
	}
	if in <= 0 || out <= 0 {
		t.Errorf("usage input=%d output=%d, want both > 0", in, out)
	}
	b, err := os.ReadFile(filepath.Join(repo, "hello.txt"))
	if err != nil {
		t.Errorf("hello.txt not written (auto-approve flag): %v", err)
	} else if !strings.Contains(string(b), "hi") {
		t.Errorf("hello.txt = %q", b)
	}
}
