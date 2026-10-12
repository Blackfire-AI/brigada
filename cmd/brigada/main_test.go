package main

import (
	"bytes"
	"fmt"
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
			wantErr:  usage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("run(%q) exit code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if got := stdout.String(); got != tt.wantOut {
				t.Errorf("run(%q) stdout = %q, want %q", tt.args, got, tt.wantOut)
			}
			if got := stderr.String(); got != tt.wantErr {
				t.Errorf("run(%q) stderr = %q, want %q", tt.args, got, tt.wantErr)
			}
		})
	}
}
