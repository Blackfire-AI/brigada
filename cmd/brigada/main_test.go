package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

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
			name:     "no arguments prints usage",
			args:     nil,
			wantCode: 0,
			wantOut:  usage,
		},
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
