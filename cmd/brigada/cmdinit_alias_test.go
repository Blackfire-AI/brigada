package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func initAliasRepo(t *testing.T, origin string) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return root
}

func TestInitAliasResolution(t *testing.T) {
	cases := []struct{ name, origin, config, transport, canon string }{
		{"user default port", "git@build:org/repo.git", "hostname example.com\nport 22\nuser ignored\n", "ssh://git@example.com/org/repo.git", "example.com/org/repo"},
		{"no user custom port", "ssh://build/org/repo", "hostname example.com\nport 2222\nidentityfile ignored\n", "ssh://example.com:2222/org/repo", "example.com:2222/org/repo"},
		{"user custom port", "ssh://git@build/org/repo", "hostname example.com\nport 2222\n", "ssh://git@example.com:2222/org/repo", "example.com:2222/org/repo"},
		{"no user default port", "build:org/repo", "hostname example.com\nport 22\n", "ssh://example.com/org/repo", "example.com/org/repo"},
		{"normalized default port", "build:org/repo", "hostname example.com\nport 022\n", "ssh://example.com/org/repo", "example.com/org/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := initAliasRepo(t, tc.origin)
			old := initSSHConfig
			initSSHConfig = func(context.Context, string) ([]byte, error) { return []byte(tc.config), nil }
			t.Cleanup(func() { initSSHConfig = old })
			got, err := selectInitOrigin(t.Context(), root, "")
			if err != nil || got != (initRepoSelection{transport: tc.transport, canon: tc.canon, alias: "build"}) {
				t.Fatalf("selection = %+v, %v", got, err)
			}
		})
	}
}

func TestInitNonSSHSingleWordHostDoesNotResolve(t *testing.T) {
	old := initSSHConfig
	calls := 0
	initSSHConfig = func(context.Context, string) ([]byte, error) {
		calls++
		return []byte("hostname example.com\nport 22\n"), nil
	}
	t.Cleanup(func() { initSSHConfig = old })
	for _, origin := range []string{
		"https://gitserver/org/repo",
		"http://gitserver/org/repo",
		"git://gitserver/org/repo",
		"file://gitserver/org/repo",
		"other://gitserver/org/repo",
	} {
		t.Run(origin, func(t *testing.T) {
			_, err := selectInitOrigin(t.Context(), initAliasRepo(t, origin), "")
			if err == nil || !strings.Contains(err.Error(), "--repo") {
				t.Fatalf("non-SSH origin %q: %v", origin, err)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("ssh -G called %d times for non-SSH origins", calls)
	}
}

func TestInitAliasRefusalAndOverride(t *testing.T) {
	root := initAliasRepo(t, "git@build:org/repo")
	old := initSSHConfig
	t.Cleanup(func() { initSSHConfig = old })
	for _, tc := range []struct {
		name, config string
		err          error
	}{
		{"ssh failure", "", fmt.Errorf("ssh failed")},
		{"unresolved", "hostname build\nport 22\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initSSHConfig = func(context.Context, string) ([]byte, error) { return []byte(tc.config), tc.err }
			_, err := selectInitOrigin(t.Context(), root, "")
			if err == nil || !strings.Contains(err.Error(), "--repo") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, tc := range []struct{ value, transport, canon string }{
		{"ssh://git@example.com/org/repo", "ssh://git@example.com/org/repo", "example.com/org/repo"},
		{"git@example.com:org/repo", "git@example.com:org/repo", "example.com/org/repo"},
		{"https://example.com/org/repo", "https://example.com/org/repo", "example.com/org/repo"},
		{"example.com/org/repo", "ssh://git@example.com/org/repo", "example.com/org/repo"},
		{"example.com:2222/org/repo", "ssh://git@example.com:2222/org/repo", "example.com:2222/org/repo"},
		{"Example.com/org/repo", "ssh://git@example.com/org/repo", "example.com/org/repo"},
		{"example.com/org/repo/", "ssh://git@example.com/org/repo", "example.com/org/repo"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := selectInitOrigin(t.Context(), root, tc.value)
			if err != nil || got.transport != tc.transport || got.canon != tc.canon || !got.override {
				t.Fatalf("selection = %+v, %v", got, err)
			}
		})
	}
	https := initAliasRepo(t, "https://example.com/org/repo")
	for _, value := range []string{"example.com/org/repo", "Example.com/org/repo", "example.com/org/repo/"} {
		if _, err := selectInitOrigin(t.Context(), https, value); err == nil || !strings.Contains(err.Error(), "full URL") {
			t.Fatalf("bare https override %q: %v", value, err)
		}
	}
	if _, err := selectInitOrigin(t.Context(), root, "example.com/org/repo.git"); err == nil || !strings.Contains(err.Error(), "full URL") {
		t.Fatalf("bare .git identity: %v", err)
	}
	ssh := initAliasRepo(t, "ssh://git@build/old/path")
	if got, err := selectInitOrigin(t.Context(), ssh, "example.com/org/repo"); err != nil || got.transport != "ssh://git@example.com/org/repo" {
		t.Fatalf("ssh URL override = %+v, %v", got, err)
	}
	for _, origin := range []string{"ssh://localhost/org/repo", "git@example.com:org/repo"} {
		t.Run(origin, func(t *testing.T) {
			got, err := selectInitOrigin(t.Context(), initAliasRepo(t, origin), "")
			if err != nil || got.transport != origin || got.alias != "" {
				t.Fatalf("selection = %+v, %v", got, err)
			}
		})
	}
	for _, origin := range []string{"Example.com/org/repo", "example.com/org/repo/"} {
		if _, err := selectInitOrigin(t.Context(), initAliasRepo(t, origin), ""); err == nil || !strings.Contains(err.Error(), "--repo") {
			t.Fatalf("transportless origin %q: %v", origin, err)
		}
	}
	fileOrigin := "file:" + filepath.Join(t.TempDir(), "remote.git")
	got, err := selectInitOrigin(t.Context(), initAliasRepo(t, fileOrigin), "")
	if err != nil || got.transport != fileOrigin || got.alias != "" {
		t.Fatalf("file origin = %+v, %v", got, err)
	}
	for _, origin := range []string{"user:s3cr3t@build:org/repo", "ssh://user:s3cr3t@build/org/repo"} {
		if _, err := selectInitOrigin(t.Context(), initAliasRepo(t, origin), ""); err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Fatalf("credential-like origin %q: %v", engine.RedactRepoURL(origin), err)
		}
	}
}

func TestParentAliasChild(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "child")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@build:org/repo"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	old := initSSHConfig
	initSSHConfig = func(context.Context, string) ([]byte, error) { return []byte("hostname example.com\nport 2222\n"), nil }
	t.Cleanup(func() { initSSHConfig = old })
	skipped, err := scanParentWithAliases(t.Context(), parent, false)
	if err != nil || len(skipped) != 1 || !strings.Contains(skipped[0].skip, "--repo") {
		t.Fatalf("skipped = %+v, %v", skipped, err)
	}
	accepted, err := scanParentWithAliases(t.Context(), parent, true)
	if err != nil || len(accepted) != 1 || accepted[0].skip != "" || accepted[0].origin != "ssh://git@example.com:2222/org/repo" {
		t.Fatalf("accepted = %+v, %v", accepted, err)
	}
}

func TestConfirmInitSelection(t *testing.T) {
	selection := initRepoSelection{transport: "ssh://git@example.com/org/repo", canon: "example.com/org/repo", alias: "build"}
	for _, tc := range []struct {
		name, answer     string
		yes, probe, want bool
	}{
		{"confirm", "yes\n", false, false, true},
		{"decline", "n\n", false, false, false},
		{"yes", "", true, false, true},
		{"probe", "", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := confirmInitSelection(selection, tc.yes, tc.probe, bufio.NewReader(strings.NewReader(tc.answer)), &out)
			if err != nil || got != tc.want || !strings.Contains(out.String(), engine.RedactRepoURL(selection.transport)) || !strings.Contains(out.String(), selection.canon) || !strings.Contains(out.String(), selection.alias) {
				t.Fatalf("accepted=%v, err=%v, output=%q", got, err, out.String())
			}
		})
	}
}

func TestConfirmInitRepoOverride(t *testing.T) {
	s := initRepoSelection{transport: "ssh://git@example.com/org/repo", canon: "example.com/org/repo", override: true}
	var out bytes.Buffer
	accepted, err := confirmInitSelection(s, false, false, bufio.NewReader(strings.NewReader("n\n")), &out)
	if err != nil || accepted || !strings.Contains(out.String(), "--repo selects") {
		t.Fatalf("accepted=%v err=%v output=%q", accepted, err, out.String())
	}
	out.Reset()
	accepted, err = confirmInitSelection(s, true, false, bufio.NewReader(strings.NewReader("")), &out)
	if err != nil || !accepted {
		t.Fatalf("--yes accepted=%v err=%v", accepted, err)
	}
}

func TestInitRegistrationTransportStored(t *testing.T) {
	binary := testsupport.BuildBrigada(t)
	oldBinary := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = oldBinary })
	for _, tc := range []struct{ name, origin, override, want string }{
		{"alias", "git@build:org/repo.git", "", "ssh://git@example.com/org/repo.git"},
		{"repo identity", "git@build:org/repo.git", "example.com/org/repo", "ssh://git@example.com/org/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initAliasRepo(t, tc.origin)
			t.Chdir(root)
			old := initSSHConfig
			initSSHConfig = func(context.Context, string) ([]byte, error) { return []byte("hostname example.com\nport 22\n"), nil }
			t.Cleanup(func() { initSSHConfig = old })
			fakeInitPATH(t, "claude", "codex")
			state := testsupport.ShortStateDir(t)
			t.Cleanup(func() {
				var out, stderr bytes.Buffer
				if code := run(context.Background(), []string{"stop", "--engine", "--state-dir", state}, &out, &stderr); code != 0 {
					t.Errorf("stop init processes: %d %s %s", code, out.String(), stderr.String())
				}
			})
			args := []string{"--yes", "--agent", "harness=claude,model=sonnet", "--agent", "harness=codex,model=gpt-5", "--signed-in", "claude,codex", "--foreman", "claude/sonnet@high", "--check", "none", "--tools", "none"}
			if tc.override != "" {
				args = append(args, "--repo", tc.override)
			}
			code, out, stderr := initFixedParallel(t, state, "", args...)
			if code != 0 {
				t.Fatalf("init: %d\n%s\n%s", code, out, stderr)
			}
			content, err := os.ReadFile(engine.ProjectConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := engine.ParseProjectConfig(content)
			if err != nil {
				t.Fatalf("project.yaml repo: %+v %v", cfg, err)
			}
			if cfg.Repo != "example.com/org/repo" {
				t.Fatalf("project.yaml repo = %q, want canonical identity", cfg.Repo)
			}
			st, err := store.Open(t.Context(), filepath.Join(state, "store.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			var identity, transport string
			err = st.View(t.Context(), func(tx *sql.Tx) error {
				return tx.QueryRow("SELECT repo_url, transport_url FROM projects").Scan(&identity, &transport)
			})
			if err != nil {
				t.Fatal(err)
			}
			if identity != "example.com/org/repo" {
				t.Fatalf("stored repo_url = %q, want canonical identity", identity)
			}
			if transport != tc.want {
				t.Fatalf("stored transport_url = %q, want %q", transport, tc.want)
			}
			if id, err := engine.Canonicalize(transport); err != nil || id != identity {
				t.Fatalf("transport canonicalizes to %q: %v", id, err)
			}
			bare := filepath.Join(t.TempDir(), "remote.git")
			if out, err := exec.Command("git", "init", "--bare", "-q", bare).CombinedOutput(); err != nil {
				t.Fatalf("bare repo: %v: %s", err, out)
			}
			script := filepath.Join(t.TempDir(), "ssh")
			if err := os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\nexec git upload-pack %q\n", bare)), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("git", "ls-remote", transport)
			cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("stored transport cannot fetch: %v: %s", err, out)
			}
		})
	}
}

func TestInitAliasConfirmationKeepsLaterInput(t *testing.T) {
	root := initAliasRepo(t, "git@build:org/repo")
	t.Chdir(root)
	old := initSSHConfig
	initSSHConfig = func(context.Context, string) ([]byte, error) { return []byte("hostname example.com\nport 22\n"), nil }
	t.Cleanup(func() { initSSHConfig = old })
	fakeInitPATH(t, "claude", "codex")
	code, out, stderr := initFixedParallel(t, testsupport.ShortStateDir(t), "yes\nn\n",
		"--agent", "harness=claude,model=sonnet",
		"--agent", "harness=codex,model=gpt-5",
		"--signed-in", "claude,codex",
		"--foreman", "claude/sonnet@high",
		"--check", "none", "--tools", "none")
	if code != 1 || !strings.Contains(out, "SSH alias build resolves") || !strings.Contains(out, "Declined; nothing written or started.") || stderr != "" {
		t.Fatalf("code=%d output=%q stderr=%q", code, out, stderr)
	}
}

func TestParentAliasRegistrationStored(t *testing.T) {
	binary := testsupport.BuildBrigada(t)
	oldBinary := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = oldBinary })
	parent := t.TempDir()
	parentRepo(t, parent, "child", "git@build:org/repo")
	t.Chdir(parent)
	old := initSSHConfig
	initSSHConfig = func(context.Context, string) ([]byte, error) { return []byte("hostname example.com\nport 2222\n"), nil }
	t.Cleanup(func() { initSSHConfig = old })
	state := testsupport.ShortStateDir(t)
	t.Cleanup(func() {
		var out, stderr bytes.Buffer
		if code := run(context.Background(), []string{"stop", "--engine", "--state-dir", state}, &out, &stderr); code != 0 {
			t.Errorf("stop parent engine: %d %s %s", code, out.String(), stderr.String())
		}
	})
	code, out, stderr := initDo(t, state, "", "--all", "--yes", "--no-wiring", "--foreman", "claude/sonnet")
	if code != 0 {
		t.Fatalf("parent init: %d\n%s\n%s", code, out, stderr)
	}
	st, err := store.Open(t.Context(), filepath.Join(state, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var identity, transport string
	err = st.View(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT repo_url, transport_url FROM projects").Scan(&identity, &transport)
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity != "example.com:2222/org/repo" {
		t.Fatalf("stored repo_url = %q, want canonical identity", identity)
	}
	if transport != "ssh://git@example.com:2222/org/repo" {
		t.Fatalf("stored transport_url = %q", transport)
	}
}
