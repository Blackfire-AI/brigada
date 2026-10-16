package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The fake harness is the parent test process; a child runs the real command
// dispatch and speaks newline-delimited MCP over its actual stdin and stdout.
func TestMCPCommandProcess(t *testing.T) {
	if os.Getenv("BRIGADA_COMMAND_TEST_CHILD") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("BRIGADA_COMMAND_TEST_ARGS")), &args); err != nil {
		os.Exit(99)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, args, os.Stdout, os.Stderr))
}

type commandBridge struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr bytes.Buffer
	waited bool
}

func commandChild(t *testing.T, args []string, session string) *commandBridge {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(args)
	cmd := exec.Command(exe, "-test.run=^TestMCPCommandProcess$")
	// A bad foreman env token proves --foreman gets the local client credential.
	cmd.Env = append(os.Environ(), "BRIGADA_COMMAND_TEST_CHILD=1", "BRIGADA_COMMAND_TEST_ARGS="+string(data), "CLAUDE_CODE_SESSION_ID="+session, "BRIGADA_TOKEN=must-not-be-used")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &commandBridge{cmd: cmd, in: in, out: bufio.NewReader(out)}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		if !p.waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return p
}
func (p *commandBridge) call(t *testing.T, id int, name string, args, meta any) (string, bool) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args, "_meta": meta}}
	if err := json.NewEncoder(p.in).Encode(body); err != nil {
		t.Fatal(err)
	}
	type result struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error any `json:"error"`
	}
	got := make(chan []byte, 1)
	go func() { line, _ := p.out.ReadBytes('\n'); got <- line }()
	var line []byte
	select {
	case line = <-got:
	case <-time.After(8 * time.Second):
		t.Fatal("MCP command timed out")
	}
	var res result
	if err := json.Unmarshal(line, &res); err != nil || res.Error != nil || len(res.Result.Content) != 1 {
		t.Fatalf("MCP answer %s err=%v", line, err)
	}
	return res.Result.Content[0].Text, res.Result.IsError
}
func (p *commandBridge) wait(t *testing.T) {
	t.Helper()
	p.waited = true
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("child command failed: %v, stderr: %s", err, p.stderr.String())
	}
}
func holdCount(t *testing.T, e *attachEnv, project int64) int {
	t.Helper()
	var n int
	if err := e.st.View(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM interactive_holds WHERE state='live' AND project_id=?", project).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestForemanCommandMetadataEOFAndSignal(t *testing.T) {
	for _, ending := range []string{"EOF", "signal"} {
		t.Run(ending, func(t *testing.T) {
			e := newAttachEnv(t, "https://example.invalid/command.git", "claude", "sonnet")
			p := commandChild(t, []string{"mcp", "--state-dir", e.stateDir, "--foreman", "git@EXAMPLE.invalid:command.git"}, "cli-session")
			text, bad := p.call(t, 1, "decision_note", map[string]any{"text": "Remember the choice."}, nil)
			if bad {
				t.Fatal(text)
			}
			if holdCount(t, e, e.project) != 1 {
				t.Fatal("command did not acquire")
			}
			var source, id, instance string
			if err := e.st.View(context.Background(), func(tx *sql.Tx) error {
				return tx.QueryRow("SELECT harness_session_id,harness_session_source,bridge_instance FROM interactive_holds WHERE state='live'").Scan(&id, &source, &instance)
			}); err != nil {
				t.Fatal(err)
			}
			if id != "cli-session" || source != "env" || instance == "" || instance == id {
				t.Fatalf("metadata %q %q %q", id, source, instance)
			}
			if ending == "EOF" {
				p.in.Close()
			} else {
				if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			p.wait(t)
			if holdCount(t, e, e.project) != 0 {
				t.Fatal("bridge did not release on shutdown")
			}
		})
	}
}

func TestHoldYieldSeparateProcessBoundOnly(t *testing.T) {
	e := newAttachEnv(t, "https://example.invalid/one.git", "claude", "sonnet")
	// A different bridge holds a different project, even though both share the
	// same harness parent process in this fake harness.
	if err := e.st.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (repo_url,default_branch,settings,foreman_harness,foreman_model,created_at,updated_at) VALUES ('https://example.invalid/two.git','main','{}','claude','sonnet',?,?)`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	first := commandChild(t, []string{"mcp", "--state-dir", e.stateDir, "--foreman", "https://example.invalid/one"}, "first-session")
	second := commandChild(t, []string{"mcp", "--state-dir", e.stateDir, "--foreman", "https://example.invalid/two"}, "second-session")
	for _, p := range []*commandBridge{first, second} {
		if text, bad := p.call(t, 1, "hold_acquire", map[string]any{}, nil); bad {
			t.Fatal(text)
		}
	}
	hook := commandChild(t, []string{"hold", "yield", "--state-dir", e.stateDir, "--session", "first-session"}, "")
	hook.wait(t)
	if holdCount(t, e, 1) != 0 || holdCount(t, e, 2) != 1 {
		t.Fatal("hook yielded the wrong hold")
	}
	// Session-only, unrelated and broken invocations all remain hook-safe.
	for _, args := range [][]string{
		{"--state-dir", e.stateDir, "--session", "unknown"},
		{"--state-dir", e.stateDir, "--unknown"},
		{"--state-dir", e.stateDir, "--config", filepath.Join(e.stateDir, "missing-config")},
		{"--state-dir", e.stateDir, "unexpected"},
	} {
		p := commandChild(t, append([]string{"hold", "yield"}, args...), "")
		p.wait(t)
		if holdCount(t, e, 2) != 1 {
			t.Fatalf("failed hook %v released another hold", args)
		}
	}
	if holdCount(t, e, 2) != 1 {
		t.Fatal("failed hook released another hold")
	}
	data, err := os.ReadFile(filepath.Join(e.stateDir, "hold-yield.log"))
	if err != nil || !strings.Contains(string(data), "hold yield") {
		t.Fatalf("missing hook failure log: %v %s", err, data)
	}
}

func TestMCPSelectorsMutuallyExclusive(t *testing.T) {
	for _, args := range [][]string{{"--foreman", "--client"}, {"--coordinator", "--foreman"}, {"--coordinator", "--client"}} {
		var out, errout bytes.Buffer
		if code := runMCP(context.Background(), args, &out, &errout); code != 2 || !strings.Contains(errout.String(), "mutually exclusive") {
			t.Fatalf("%v code=%d stderr=%s", args, code, errout.String())
		}
	}
}
