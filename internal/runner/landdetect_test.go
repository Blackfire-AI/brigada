package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/foreman"
)

type landedTransport func(*http.Request) (*http.Response, error)

func (f landedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// landedGit runs git in dir for the trailer scan test, failing it on
// error.
func landedGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=brigada test",
		"-c", "user.email=test@brigada.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (in %s): %v: %s", args, dir, err, out)
	}
	return string(out)
}

// TestReportLandedCarriesTrailerFields: the scan reports every commit
// carrying a Brigada-Task trailer, newest first per id, each with its
// Brigada-Branch trailer's branch and head — a reused id carries both
// its old and its new candidate (§7.1).
func TestReportLandedCarriesTrailerFields(t *testing.T) {
	dir := t.TempDir()
	landedGit(t, dir, "init", "-q", ".")
	landedGit(t, dir, "checkout", "-q", "-b", "main")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("old.txt", "old\n")
	landedGit(t, dir, "add", "-A")
	landedGit(t, dir, "commit", "-qm", "the previous store's task four\n\nBrigada-Task: 4\nBrigada-Branch: brigada/task-4@0e31d17a")
	old := strings.TrimSpace(landedGit(t, dir, "rev-parse", "HEAD"))
	write("new.txt", "new\n")
	landedGit(t, dir, "add", "-A")
	landedGit(t, dir, "commit", "-qm", "this store's task four\n\nBrigada-Task: 4\nBrigada-Branch: brigada/task-4-r4@8e37d19e")
	newer := strings.TrimSpace(landedGit(t, dir, "rev-parse", "HEAD"))
	write("bare.txt", "bare\n")
	landedGit(t, dir, "add", "-A")
	landedGit(t, dir, "commit", "-qm", "task seven, no branch trailer\n\nBrigada-Task: 7")
	bare := strings.TrimSpace(landedGit(t, dir, "rev-parse", "HEAD"))

	var got struct {
		RepoURL string                `json:"repo_url"`
		Branch  string                `json:"branch"`
		Landed  []engine.LandedCommit `json:"landed"`
	}
	client := api.NewClientWithHTTPClient(&http.Client{Transport: landedTransport(func(req *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"recorded":0}`)), Header: make(http.Header)}, nil
	})}, "test-token")

	if err := ReportLanded(context.Background(), client, "https://example.invalid/brigada.git", "main",
		foreman.Checkout{Path: dir}); err != nil {
		t.Fatal(err)
	}

	if got.RepoURL != "https://example.invalid/brigada.git" || got.Branch != "main" {
		t.Fatalf("report = %s %s, want the repo and branch", got.RepoURL, got.Branch)
	}
	byTask := map[int64][]engine.LandedCommit{}
	for _, lc := range got.Landed {
		byTask[lc.TaskID] = append(byTask[lc.TaskID], lc)
	}
	if len(byTask) != 2 {
		t.Fatalf("reported tasks = %v, want 4 and 7", got.Landed)
	}
	want4 := []engine.LandedCommit{
		{TaskID: 4, Commit: newer, Branch: "brigada/task-4-r4", Head: "8e37d19e"},
		{TaskID: 4, Commit: old, Branch: "brigada/task-4", Head: "0e31d17a"},
	}
	if len(byTask[4]) != 2 {
		t.Fatalf("task 4 candidates = %v, want the new and the old", byTask[4])
	}
	for i := range want4 {
		if byTask[4][i] != want4[i] {
			t.Errorf("task 4 candidate %d = %+v, want %+v (newest first)", i, byTask[4][i], want4[i])
		}
	}
	if len(byTask[7]) != 1 {
		t.Fatalf("task 7 candidates = %v, want one", byTask[7])
	}
	if byTask[7][0] != (engine.LandedCommit{TaskID: 7, Commit: bare}) {
		t.Errorf("task 7 candidate = %+v, want the commit with empty branch and head", byTask[7][0])
	}
}
