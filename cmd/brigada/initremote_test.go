package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func remoteRepo(t *testing.T, remotes map[string]string) string {
	t.Helper()
	root := initAliasRepo(t, "https://example.com/old/repo")
	gitIn(t, root, "remote", "remove", "origin")
	for name, url := range remotes {
		gitIn(t, root, "remote", "add", name, url)
	}
	return root
}

func TestInitRemoteChoice(t *testing.T) {
	ctx := t.Context()
	one := remoteRepo(t, map[string]string{"upstream": "https://example.com/team/one.git"})
	got, name, err := selectInitOriginWithRemote(ctx, one, "", "", nil)
	if err != nil || name != "upstream" || got.canon != "example.com/team/one" || got.transport != "https://example.com/team/one.git" {
		t.Fatalf("single remote: %+v %q %v", got, name, err)
	}
	withOrigin := remoteRepo(t, map[string]string{"origin": "https://example.com/team/main", "upstream": "https://example.com/team/other"})
	got, name, err = selectInitOriginWithRemote(ctx, withOrigin, "", "", nil)
	if err != nil || name != "origin" || got.canon != "example.com/team/main" {
		t.Fatalf("preferred origin: %+v %q %v", got, name, err)
	}
	got, name, err = selectInitOriginWithRemote(ctx, withOrigin, "", "upstream", nil)
	if err != nil || name != "upstream" || got.canon != "example.com/team/other" {
		t.Fatalf("explicit remote: %+v %q %v", got, name, err)
	}
	many := remoteRepo(t, map[string]string{"upstream": "https://example.com/team/one", "backup": "https://example.com/team/two"})
	if _, _, err = selectInitOriginWithRemote(ctx, many, "", "", nil); err == nil || !strings.Contains(err.Error(), "--remote") {
		t.Fatalf("noninteractive ambiguity: %v", err)
	}
	asked := false
	got, name, err = selectInitOriginWithRemote(ctx, many, "", "", func(options []gitRemote) (string, error) {
		asked = true
		if len(options) != 2 {
			t.Fatalf("offered %d remotes", len(options))
		}
		return "backup", nil
	})
	if err != nil || !asked || name != "backup" || got.canon != "example.com/team/two" {
		t.Fatalf("interactive choice: %+v %q %v", got, name, err)
	}
	if _, _, err = selectInitOriginWithRemote(ctx, many, "", "missing", nil); err == nil || !strings.Contains(err.Error(), "--remote") {
		t.Fatalf("missing --remote name: %v", err)
	}
}

func TestInitRemoteQuestion(t *testing.T) {
	root := remoteRepo(t, map[string]string{"upstream": "https://example.com/team/one", "backup": "https://example.com/team/two"})
	t.Chdir(root)
	fakeInitPATH(t, "claude", "codex")
	code, out, stderr := initFixedParallel(t, testsupport.ShortStateDir(t), "backup\nn\n",
		"--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5",
		"--signed-in", "claude,codex", "--foreman", "claude/sonnet@high", "--check", "none", "--tools", "none")
	if code != 1 || stderr != "" || !strings.Contains(out, "Remote name (--remote NAME):") || !strings.Contains(out, "Using git remote backup") || !strings.Contains(out, "brigada init: set up example.com/team/two") || !strings.Contains(out, "Declined; nothing written or started.") {
		t.Fatalf("interactive remote choice: %d\n%s\n%s", code, out, stderr)
	}
	code, _, stderr = initFixedParallel(t, testsupport.ShortStateDir(t), "", "--probe")
	if code != 1 || !strings.Contains(stderr, "--remote") {
		t.Fatalf("noninteractive remote choice: %d %s", code, stderr)
	}
}

func TestInitSingleRemoteShown(t *testing.T) {
	root := remoteRepo(t, map[string]string{"upstream": "https://example.com/team/one"})
	t.Chdir(root)
	fakeInitPATH(t, "claude", "codex")
	code, out, stderr := initFixedParallel(t, testsupport.ShortStateDir(t), "", "--probe",
		"--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5",
		"--signed-in", "claude,codex")
	if code != 0 || !strings.Contains(out, "Using git remote upstream") || !strings.Contains(out, "example.com/team/one") {
		t.Fatalf("single remote probe: %d\n%s\n%s", code, out, stderr)
	}
}

func TestGitOriginURLRemoteChoice(t *testing.T) {
	one := remoteRepo(t, map[string]string{"upstream": "https://example.com/team/one.git"})
	if got, err := gitOriginURL(one); err != nil || got != "https://example.com/team/one.git" {
		t.Fatalf("single non-origin remote: %q %v", got, err)
	}
	withOrigin := remoteRepo(t, map[string]string{"origin": "https://example.com/team/main.git", "upstream": "https://example.com/team/other.git"})
	if got, err := gitOriginURL(withOrigin); err != nil || got != "https://example.com/team/main.git" {
		t.Fatalf("origin preferred: %q %v", got, err)
	}
	if _, err := gitOriginURL(remoteRepo(t, nil)); err == nil || !strings.Contains(err.Error(), "no remote") {
		t.Fatalf("no remote: %v", err)
	}
	if _, err := gitOriginURL(remoteRepo(t, map[string]string{"upstream": "https://example.com/team/one", "backup": "https://example.com/team/two"})); err == nil || !strings.Contains(err.Error(), "--repo") {
		t.Fatalf("multiple non-origin remotes: %v", err)
	}
	alias := remoteRepo(t, map[string]string{"origin": "git@build:team/repo"})
	old := initSSHConfig
	initSSHConfig = func(context.Context, string) ([]byte, error) { return []byte("hostname example.com\nport 2222\n"), nil }
	t.Cleanup(func() { initSSHConfig = old })
	if got, err := gitOriginURL(alias); err != nil || got != "ssh://git@example.com:2222/team/repo" {
		t.Fatalf("alias origin: %q %v", got, err)
	}
	initSSHConfig = func(context.Context, string) ([]byte, error) { return nil, context.DeadlineExceeded }
	if got, err := gitOriginURL(alias); err != nil || got != "git@build:team/repo" {
		t.Fatalf("failed alias resolution: %q %v", got, err)
	}
	initSSHConfig = func(context.Context, string) ([]byte, error) {
		t.Fatal("ssh -G called for HTTPS remote")
		return nil, nil
	}
	https := remoteRepo(t, map[string]string{"origin": "https://gitserver/team/repo"})
	if got, err := gitOriginURL(https); err != nil || got != "https://gitserver/team/repo" {
		t.Fatalf("HTTPS single-word host: %q %v", got, err)
	}
}

func TestParentSkipsRemoteChoice(t *testing.T) {
	parent := t.TempDir()
	child := parentRepo(t, parent, "child", "")
	gitIn(t, child, "remote", "add", "upstream", "https://example.com/team/one")
	gitIn(t, child, "remote", "add", "backup", "https://example.com/team/two")
	children, err := scanParent(t.Context(), parent)
	if err != nil || len(children) != 1 || !strings.Contains(children[0].skip, "--remote") {
		t.Fatalf("parent scan: %+v %v", children, err)
	}
	sole := parentRepo(t, parent, "sole", "")
	gitIn(t, sole, "remote", "add", "upstream", "https://example.com/team/sole")
	children, err = scanParent(t.Context(), parent)
	if err != nil || len(children) != 2 || children[1].skip != "" || children[1].remote != "upstream" || children[1].canon != "example.com/team/sole" || children[1].origin != "https://example.com/team/sole" {
		t.Fatalf("parent sole remote: %+v %v", children, err)
	}
}
