package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/mcp"
	"github.com/Blackfire-AI/brigada/internal/version"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{
			name:     "short help flag prints usage",
			args:     []string{"-h"},
			wantCode: 0,
			wantOut:  usage,
		},
		{
			name:     "long help flag prints usage",
			args:     []string{"--help"},
			wantCode: 0,
			wantOut:  usage,
		},
		{
			name:     "help command prints usage",
			args:     []string{"help"},
			wantCode: 0,
			wantOut:  usage,
		},
		{
			name:     "version prints the binary version",
			args:     []string{"version"},
			wantCode: 0,
			wantOut:  fmt.Sprintf("brigada %s\n", version.Version),
		},
		{
			name:     "unknown command prints usage to stderr",
			args:     []string{"frobnicate"},
			wantCode: 2,
			wantErr:  "brigada: unknown command \"frobnicate\"\n\n" + usage,
		},
		{
			name:     "task without a subcommand is a usage error",
			args:     []string{"task"},
			wantCode: 2,
			wantErr:  "usage: brigada task submit",
		},
		{
			name:     "cancel without a job id is a usage error",
			args:     []string{"cancel"},
			wantCode: 2,
			wantErr:  "usage: brigada cancel <job-id>",
		},
		{
			name:     "cancel with a non-numeric id is a usage error",
			args:     []string{"cancel", "seven"},
			wantCode: 2,
			wantErr:  "job id must be a positive integer",
		},
		{
			name:     "task submit without a repo is a usage error",
			args:     []string{"task", "submit"},
			wantCode: 2,
			wantErr:  "task submit needs --repo URL",
		},
		{
			name:     "task submit without a spec is a usage error",
			args:     []string{"task", "submit", "--repo", "https://example.invalid/x.git"},
			wantCode: 2,
			wantErr:  "task submit needs --spec FILE",
		},
		{
			name:     "project without a subcommand is a usage error",
			args:     []string{"project"},
			wantCode: 2,
			wantErr:  "usage: brigada project foreman",
		},
		{
			name:     "unknown project subcommand is a usage error",
			args:     []string{"project", "frob"},
			wantCode: 2,
			wantErr:  "unknown project subcommand",
		},
		{
			name:     "project foreman without a repo is a usage error",
			args:     []string{"project", "foreman"},
			wantCode: 2,
			wantErr:  "project foreman needs --repo URL",
		},
		{
			name:     "project foreman with only a harness is a usage error",
			args:     []string{"project", "foreman", "--repo", "https://example.invalid/x.git", "--harness", "claude"},
			wantCode: 2,
			wantErr:  "sets --harness and --model together",
		},
		{
			name:     "project foreman with only a model is a usage error",
			args:     []string{"project", "foreman", "--repo", "https://example.invalid/x.git", "--model", "opus"},
			wantCode: 2,
			wantErr:  "sets --harness and --model together",
		},
		{
			name:     "mcp without a socket is a usage error",
			args:     []string{"mcp"},
			wantCode: 2,
			wantErr:  "mcp needs --socket PATH",
		},
		{
			name:     "attach with positional arguments is a usage error",
			args:     []string{"attach", "extra"},
			wantCode: 2,
			wantErr:  "attach takes no positional arguments",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("run(%q) exit code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if tt.wantOut != "" {
				if got := stdout.String(); got != tt.wantOut {
					t.Errorf("run(%q) stdout = %q, want %q", tt.args, got, tt.wantOut)
				}
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("run(%q) stderr = %q, want it to contain %q", tt.args, stderr.String(), tt.wantErr)
			}
		})
	}
}

// TestRunMCPNeedsToken covers the bridge's one credential rule (§12.2):
// the token comes from BRIGADA_TOKEN — the environment the harness's
// MCP config names — and never from a flag. Without it the bridge
// refuses to start: it has nothing to call the engine with.
func TestRunMCPNeedsToken(t *testing.T) {
	t.Setenv(mcp.EnvToken, "")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"mcp", "--socket", "/run/brigada/api.sock"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("mcp without a token: exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), mcp.EnvToken) {
		t.Errorf("mcp without a token: stderr = %q, want %s named in it", stderr.String(), mcp.EnvToken)
	}
	if stdout.Len() != 0 {
		t.Errorf("mcp without a token wrote to stdout: %q, want the protocol's stream untouched", stdout.String())
	}
}

// TestRunnerMovesToItsStateDir covers the daemon's home (§13): once the
// runtime resolves, the runner's working directory is its state
// directory, so children never inherit a launcher's directory that can
// vanish. The agent file check fails fast, after the move.
func TestRunnerMovesToItsStateDir(t *testing.T) {
	t.Chdir(t.TempDir())
	// A short path: t.TempDir under the test's name overruns the socket
	// path cap, and resolveRuntime would refuse before the move.
	st, err := os.MkdirTemp("", "bs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(st) })
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"runner", "--state-dir", st, "--agent", "harness=kimi,agent-file=/no/such/file"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("runner with a missing agent file: exit code = %d, want 1; stderr = %q", code, stderr.String())
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// SameFile, not string equality: macOS resolves /var to /private/var.
	got, err := os.Stat(cwd)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(st)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(got, want) {
		t.Errorf("working directory = %q, want the state directory %q", cwd, st)
	}
}

// TestEngineRefusesAStateDirItCannotEnter covers the move's failure: a
// state directory that is not a directory stops the engine at once,
// naming it.
func TestEngineRefusesAStateDirItCannotEnter(t *testing.T) {
	t.Chdir(t.TempDir())
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"engine", "--state-dir", notDir}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("engine with a file as state dir: exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "state directory") {
		t.Errorf("stderr = %q, want the state directory mentioned", stderr.String())
	}
}
