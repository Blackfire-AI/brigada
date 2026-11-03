package runner

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/adapter/opencode"
	"github.com/Blackfire-AI/brigada/internal/api"
)

func workGrant() *api.JobGrant { return &api.JobGrant{Kind: "work", Prompt: "p"} }

func TestShellScopeNoteClaudeDefault(t *testing.T) {
	t.Parallel()
	got := shellScopeNote(&claude.Adapter{}, workGrant(), nil, "")
	for _, want := range []string{
		"`go build`, `go test`, `go vet`, `gofmt`.",
		"worktree root",
		claudeCompoundNote,
		"covers that command, not the shell",
		"the runner commits and pushes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the default claude scope's paragraph lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, opencodeCompoundNote) {
		t.Errorf("claude's paragraph carries opencode's compound rule:\n%s", got)
	}
}

// TestClaudeCompoundNoteMatchesTheProbes pins the compound sentence to
// the results of running each form under the Go default scope: every
// form that ran is named, and git, which was refused, is not promised.
func TestClaudeCompoundNoteMatchesTheProbes(t *testing.T) {
	t.Parallel()
	for _, form := range []string{"&&", ";", "pipe into tail", "2>&1", "cd persists", "worktree root"} {
		if !strings.Contains(claudeCompoundNote, form) {
			t.Errorf("the compound sentence dropped %q: %s", form, claudeCompoundNote)
		}
	}
	if strings.Contains(claudeCompoundNote, "git") {
		t.Errorf("the compound sentence promises git: %s", claudeCompoundNote)
	}
}

func TestShellScopeNoteClaudeExplicitScope(t *testing.T) {
	t.Parallel()
	ad := &claude.Adapter{AllowedTools: []string{"Edit", "Bash(npm test:*)", "Bash(npx jest:*)"}}
	got := shellScopeNote(ad, workGrant(), nil, "")
	if !strings.Contains(got, "`npm test`, `npx jest`.") || strings.Contains(got, "go test") || strings.Contains(got, "Edit") {
		t.Errorf("explicit scope not named in order, or Edit leaked:\n%s", got)
	}
}

func TestShellScopeNoteClaudeNone(t *testing.T) {
	t.Parallel()
	got := shellScopeNote(&claude.Adapter{AllowedTools: []string{}}, workGrant(), nil, "")
	if !strings.Contains(got, "you have no shell") || !strings.Contains(got, "report done") || strings.Contains(got, "only these commands") {
		t.Errorf("tools=none wording wrong:\n%s", got)
	}
	// Only non-Bash tools: no shell command either.
	got = shellScopeNote(&claude.Adapter{AllowedTools: []string{"Edit"}}, workGrant(), nil, "")
	if !strings.Contains(got, "you have no shell") {
		t.Errorf("a scope with no shell command wording wrong:\n%s", got)
	}
}

func TestShellScopeNoteWholeShellGetsNone(t *testing.T) {
	t.Parallel()
	if got := shellScopeNote(&claude.Adapter{AllowedTools: []string{"Bash"}}, workGrant(), nil, ""); got != "" {
		t.Errorf("a whole-shell scope got a paragraph:\n%s", got)
	}
}

func TestShellScopeNoteOpencodeMapped(t *testing.T) {
	t.Parallel()
	tools := []string{"Bash(make test:*)", "Edit", "Bash(go *:*)", "Bash(just:*)"}
	got := shellScopeNote(&opencode.Adapter{}, workGrant(), tools, "")
	if !strings.Contains(got, "`make test`, `just`.") || strings.Contains(got, "go *") {
		t.Errorf("mapped commands wrong:\n%s", got)
	}
	if !strings.Contains(got, opencodeCompoundNote) || strings.Contains(got, claudeCompoundNote) {
		t.Errorf("opencode's compound rule missing:\n%s", got)
	}
	if def := shellScopeNote(&opencode.Adapter{}, workGrant(), nil, ""); !strings.Contains(def, "`go build`, `go test`, `go vet`, `gofmt`.") {
		t.Errorf("opencode's nil scope is not the Go default:\n%s", def)
	}
}

func TestShellScopeNoteOpencodeNothingMaps(t *testing.T) {
	t.Parallel()
	got := shellScopeNote(&opencode.Adapter{}, workGrant(), []string{"Edit", "Bash(go *:*)"}, "")
	if !strings.Contains(got, "you have no shell") {
		t.Errorf("an unmappable scope should read as no shell:\n%s", got)
	}
}

func TestShellScopeNoteHarnessWithoutTheInterfaceIsUnchanged(t *testing.T) {
	t.Parallel()
	if got := shellScopeNote(&fake.Adapter{}, workGrant(), nil, ""); got != "" {
		t.Errorf("a harness without ShellScoped got a paragraph:\n%s", got)
	}
}

func TestShellScopeNoteSkipsReviewsAndForemanTurns(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"review", "plan_review"} {
		g := &api.JobGrant{Kind: kind, Prompt: "p"}
		if got := shellScopeNote(&claude.Adapter{}, g, nil, ""); got != "" {
			t.Errorf("a %s job got a paragraph:\n%s", kind, got)
		}
	}
	if got := shellScopeNote(&claude.Adapter{}, workGrant(), nil, "/tmp/mcp.json"); got != "" {
		t.Errorf("a job with an MCP config got a paragraph:\n%s", got)
	}
}

// promptCatcher records the prompt the runner hands the adapter and stops
// the run there; it forwards the scope so the runner sees a shell-scoped
// harness.
type promptCatcher struct {
	adapter.Adapter
	prompt *string
}

func (p promptCatcher) Command(job adapter.Job, _, _ string) (*exec.Cmd, error) {
	*p.prompt = job.Prompt
	return nil, errors.New("stop")
}

func (p promptCatcher) ShellCommands(tools []string) ([]string, bool) {
	return p.Adapter.(adapter.ShellScoped).ShellCommands(tools)
}

func TestRunWorkerAppendsTheShellScope(t *testing.T) {
	t.Parallel()
	var got string
	ad := promptCatcher{Adapter: &claude.Adapter{}, prompt: &got}
	r := &jobRun{cfg: &Config{}, grant: &api.JobGrant{Kind: "work", Prompt: "do it"}}
	_ = r.runWorker(context.Background(), ad, "", t.TempDir(), nil)
	if !strings.HasPrefix(got, "do it\nShell scope: ") || !strings.Contains(got, "`go test`") {
		t.Errorf("runWorker's prompt lacks the paragraph:\n%q", got)
	}

	review := &jobRun{cfg: &Config{}, grant: &api.JobGrant{Kind: "review", Prompt: "do it"}}
	_ = review.runWorker(context.Background(), ad, "", t.TempDir(), nil)
	if got != "do it" {
		t.Errorf("a review's prompt changed: %q", got)
	}
}
