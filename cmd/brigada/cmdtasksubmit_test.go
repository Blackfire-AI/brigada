package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// submitTransport stands in for the engine in the task submit tests
// (§12.1): it captures the submitted body and answers a fixed result.
type submitTransport struct {
	body []byte
}

func (s *submitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/v1/tasks" {
		return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
	}
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	s.body = b
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"answer":"created","project_id":1,"task_id":11,"job_id":12,"run":1}`)),
		Request:    req,
	}, nil
}

// TestTaskSubmitPassesToolGrants: `--tool-grants` is repeatable and
// carries each pattern onto the submitted task (§6.1), as the API's
// tool_grants array.
func TestTaskSubmitPassesToolGrants(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "spec.txt")
	if err := os.WriteFile(spec, []byte("do work"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	capture := &submitTransport{}
	old := connectSubmit
	connectSubmit = func(_ context.Context, _ *runtime, _ string, _ bool, _ io.Writer) (*api.Client, int) {
		return api.NewClientWithHTTPClient(&http.Client{Transport: capture}, ""), 0
	}
	t.Cleanup(func() { connectSubmit = old })

	var out, errb bytes.Buffer
	code := runTaskSubmit(context.Background(), []string{
		"--state-dir", dir, "--config", cfg,
		"--repo", "https://example.invalid/x.git", "--spec", spec,
		"--tool-grants", "Bash(git merge:*)", "--tool-grants", "Bash(git checkout:*)",
	}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	var body struct {
		ToolGrants []string `json:"tool_grants"`
	}
	if err := json.Unmarshal(capture.body, &body); err != nil {
		t.Fatalf("decode the submitted body %q: %v", capture.body, err)
	}
	if !slices.Equal(body.ToolGrants, []string{"Bash(git merge:*)", "Bash(git checkout:*)"}) {
		t.Errorf("submitted tool_grants = %v, want the two grants", body.ToolGrants)
	}
	if !strings.Contains(out.String(), "task 11 job 12") {
		t.Errorf("stdout = %q, want the created task and job", out.String())
	}
}

// TestTaskSubmitRejectsUnknownFlag: a misspelled --tool-grants is a usage
// error, as any unknown flag is.
func TestTaskSubmitRejectsUnknownFlag(t *testing.T) {
	var out, errb bytes.Buffer
	code := runTaskSubmit(context.Background(), []string{
		"--repo", "https://example.invalid/x.git", "--spec", "-",
		"--tool-grant", "Bash(git merge:*)",
	}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit %d, want 2; stderr %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "tool-grant") {
		t.Errorf("stderr %q does not name the unknown flag", errb.String())
	}
}
