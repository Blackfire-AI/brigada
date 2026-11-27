package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// probeWorkRepo makes a work repository with a local bare remote and a git
// identity, returning both paths.
func probeWorkRepo(t *testing.T) (root, remote string) {
	t.Helper()
	remote = t.TempDir()
	wireGit(t, remote, "init", "--bare", "-q")
	root = t.TempDir()
	wireGit(t, root, "init", "-q")
	wireGit(t, root, "config", "user.name", "probe test")
	wireGit(t, root, "config", "user.email", "probe@example.invalid")
	wireGit(t, root, "remote", "add", "origin", remote)
	return root, remote
}

// probeHook installs a pre-receive hook in a bare repo that runs script,
// so a test can reproduce a remote's rejection or a refused delete.
func probeHook(t *testing.T, remote, script string) {
	t.Helper()
	hook := filepath.Join(remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInitPushProbeSuccess(t *testing.T) {
	root, _ := probeWorkRepo(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wireGit(t, root, "add", "f.txt")
	wireGit(t, root, "commit", "-qm", "base")
	before := wireGit(t, root, "rev-parse", "HEAD")

	res, err := runInitPushProbe(context.Background(), root, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if res.outcome != probeOutcomeOK {
		t.Fatalf("outcome = %v, want ok (words %q)", res.outcome, res.words)
	}
	// The probe leaves no ref behind.
	if out := wireGit(t, root, "ls-remote", "origin", initProbeRefPrefix+"*"); strings.TrimSpace(out) != "" {
		t.Fatalf("probe ref left behind: %s", out)
	}
	// HEAD, index and working tree are untouched.
	if after := wireGit(t, root, "rev-parse", "HEAD"); after != before {
		t.Fatalf("HEAD moved %s -> %s", before, after)
	}
	if out := wireGit(t, root, "status", "--porcelain"); out != "" {
		t.Fatalf("working tree changed: %q", out)
	}
}

func TestInitPushProbeUnborn(t *testing.T) {
	// A fresh repository with a remote but no commits: rev-parse HEAD fails,
	// and the probe must still run against the empty tree.
	root, _ := probeWorkRepo(t)

	res, err := runInitPushProbe(context.Background(), root, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if res.outcome != probeOutcomeOK {
		t.Fatalf("outcome = %v, want ok (words %q)", res.outcome, res.words)
	}
	if out := wireGit(t, root, "ls-remote", "origin", initProbeRefPrefix+"*"); strings.TrimSpace(out) != "" {
		t.Fatalf("probe ref left behind: %s", out)
	}
	if _, _, err := probeGit(context.Background(), root, nil, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
		t.Fatal("the probe created HEAD")
	}
}

func TestInitPushProbeIdentityMissing(t *testing.T) {
	root, _ := probeWorkRepo(t)
	wireGit(t, root, "config", "--unset", "user.name")
	_, err := runInitPushProbe(context.Background(), root, "origin")
	if err == nil || !strings.Contains(err.Error(), "user.name") {
		t.Fatalf("err = %v, want it to name user.name", err)
	}
}

func TestInitPushProbeReadOnlyRejected(t *testing.T) {
	root, remote := probeWorkRepo(t)
	probeHook(t, remote, "echo \"ERROR: The key you are authenticating with is marked as read only.\" >&2\nexit 1\n")

	res, err := runInitPushProbe(context.Background(), root, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if res.outcome != probeOutcomeRefused || !strings.Contains(res.words, "marked as read only") {
		t.Fatalf("outcome = %v, words = %q; want refused with the read-only text", res.outcome, res.words)
	}
	if fix := initProbeFix(res.words); !strings.Contains(fix, "write access") {
		t.Fatalf("fix = %q, want the write-access fix", fix)
	}
}

func TestInitPushProbeGH007Rejected(t *testing.T) {
	root, remote := probeWorkRepo(t)
	probeHook(t, remote, "echo \"error: GH007: Your push would publish a private email address.\" >&2\nexit 1\n")

	res, err := runInitPushProbe(context.Background(), root, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if res.outcome != probeOutcomeRefused || !strings.Contains(res.words, "GH007") {
		t.Fatalf("outcome = %v, words = %q; want refused with GH007", res.outcome, res.words)
	}
	if fix := initProbeFix(res.words); !strings.Contains(fix, "noreply") {
		t.Fatalf("fix = %q, want the noreply email fix", fix)
	}
}

func TestInitPushProbeUnreachable(t *testing.T) {
	root, _ := probeWorkRepo(t)
	// A remote path that does not exist: git's own transport error, with no
	// `remote:`/`ERROR:` response.
	wireGit(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "absent.git"))

	res, err := runInitPushProbe(context.Background(), root, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if res.outcome != probeOutcomeUnreachable {
		t.Fatalf("outcome = %v, want unreachable (words %q)", res.outcome, res.words)
	}
}

func TestInitPushProbeDeleteRefused(t *testing.T) {
	root, remote := probeWorkRepo(t)
	// The bare repo's pre-receive refuses deletes but accepts creates, so the
	// probe's push succeeds and its delete is refused; the ref survives.
	probeHook(t, remote, "while read old new ref; do\n"+
		"  if [ \"$new\" = 0000000000000000000000000000000000000000 ]; then\n"+
		"    echo \"deletes are refused\" >&2\n    exit 1\n  fi\n"+
		"done\nexit 0\n")

	res, err := runInitPushProbe(context.Background(), root, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if res.outcome != probeOutcomeOK {
		t.Fatalf("outcome = %v, want ok (words %q)", res.outcome, res.words)
	}
	if res.deleteErr == nil {
		t.Fatal("delete did not fail; want deleteErr set")
	}
	if out := wireGit(t, root, "ls-remote", "origin", initProbeRefPrefix+"*"); !strings.Contains(out, res.ref) {
		t.Fatalf("surviving ref %s not in ls-remote: %s", res.ref, out)
	}
}

// TestInitPushProbeClassification pins the two-way sort of a failed probe
// push: the remote's own answer is a refusal, git's transport error is not.
func TestInitPushProbeClassification(t *testing.T) {
	for _, tc := range []struct {
		name, stderr string
		refused      bool
	}{
		{"read-only key", "ERROR: The key you are authenticating with has been marked as read only.\nfatal: Could not read from remote repository.", true},
		{"GH007", "remote: error: GH007: Your push would publish a private email address.\n ! [remote rejected] abc -> refs/brigada/probe/x (push declined due to email privacy restrictions)", true},
		{"permission to", "remote: Permission to o/r.git denied to someone.\nfatal: unable to access 'https://github.com/o/r.git/': The requested URL returned error: 403", true},
		{"repository not found", "remote: Repository not found.\nfatal: repository 'https://github.com/o/r.git/' not found", true},
		{"silent hook", " ! [remote rejected] abc -> refs/brigada/probe/x (pre-receive hook declined)\nerror: failed to push some refs to 'origin'", true},
		{"ssh key turned away", "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", true},
		{"https credential turned away", "fatal: Authentication failed for 'https://github.com/o/r.git/'", true},
		{"dns https", "fatal: unable to access 'https://example.invalid/r.git/': Could not resolve host: example.invalid", false},
		{"dns ssh", "ssh: Could not resolve hostname example.invalid: Name or service not known\nfatal: Could not read from remote repository.", false},
		{"connection refused", "ssh: connect to host example.invalid port 22: Connection refused\nfatal: Could not read from remote repository.", false},
		{"timeout", "fatal: unable to access 'https://example.invalid/r.git/': Failed to connect to example.invalid port 443 after 130000 ms: Connection timed out", false},
		{"tls", "fatal: unable to access 'https://example.invalid/r.git/': gnutls_handshake() failed: Error in the pull function.", false},
		{"absent path", "fatal: '/tmp/absent.git' does not appear to be a git repository\nfatal: Could not read from remote repository.", false},
	} {
		if got := probePushRefused(tc.stderr); got != tc.refused {
			t.Errorf("%s: probePushRefused = %v, want %v", tc.name, got, tc.refused)
		}
	}
	// The diagnosis table: a known signature names its fix, anything else
	// leaves the remote's verbatim words to stand alone.
	for words, want := range map[string]string{
		"ERROR: The key you are authenticating with has been marked as read only.": "write access",
		"remote: error: GH007: Your push would publish a private email address.":   "noreply",
		"remote: error: push declined due to a private email restriction":          "noreply",
		"remote: Permission to o/r.git denied to someone.":                         "scope",
		"remote: Repository not found.":                                            "",
	} {
		got := initProbeFix(words)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("initProbeFix(%q) = %q, want it to contain %q", words, got, want)
		}
	}
}

// TestInitPushProbeSilentHookRefused: a hook that rejects without a word
// still is the remote's refusal, not an unreachable remote.
func TestInitPushProbeSilentHookRefused(t *testing.T) {
	root, remote := probeWorkRepo(t)
	probeHook(t, remote, "exit 1\n")
	res, err := runInitPushProbe(context.Background(), root, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if res.outcome != probeOutcomeRefused {
		t.Fatalf("outcome = %v, want refused (words %q)", res.outcome, res.words)
	}
}

// TestInitPushProbeDeleteRefusedWarnsAndContinues: a remote that accepts the
// probe's push and refuses its delete does not fail init. Init warns with
// the surviving ref's full name and the exact command to delete it, and
// setup continues to the end.
func TestInitPushProbeDeleteRefusedWarnsAndContinues(t *testing.T) {
	binary := testsupport.BuildBrigada(t)
	old := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = old })
	importRepo(t)
	remote := initPushURL(t)
	probeHook(t, remote, "while read old new ref; do\n"+
		"  if [ \"$new\" = 0000000000000000000000000000000000000000 ]; then\n"+
		"    echo \"deletes are refused\" >&2\n    exit 1\n  fi\n"+
		"done\nexit 0\n")
	fakeInitPATH(t, "claude", "codex")
	state := testsupport.ShortStateDir(t)
	initApplyCleanup(t, state)
	code, out, err := initDo(t, state, "", "--yes", "--accept-limited",
		"--agent", "harness=claude,model=sonnet",
		"--signed-in", "claude", "--check", "true", "--tools", "none")
	if code != 0 {
		t.Fatalf("init = %d\n%s\n%s", code, out, err)
	}
	refs := strings.Fields(wireGit(t, ".", "ls-remote", remote, initProbeRefPrefix+"*"))
	if len(refs) != 2 {
		t.Fatalf("want one surviving probe ref, ls-remote: %q", refs)
	}
	ref := refs[1]
	if !strings.Contains(out, "WARNING:") || !strings.Contains(out, ref) ||
		!strings.Contains(out, "\n  git push origin --delete "+ref+"\n") {
		t.Fatalf("missing the surviving-ref warning and delete command for %s:\n%s", ref, out)
	}
	if _, e := os.Stat(filepath.Join(".brigada", "project.yaml")); e != nil {
		t.Fatalf("init did not continue past the failed delete: %v\n%s", e, out)
	}
}

func TestInitPushProbeRefusedFailsInitBeforeWrite(t *testing.T) {
	importRepo(t)
	remote := initPushURL(t)
	probeHook(t, remote, "echo \"ERROR: The key you are authenticating with is marked as read only.\" >&2\nexit 1\n")
	fakeInitPATH(t, "claude", "codex")
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	code, out, err := initDo(t, state, "", "--yes", "--accept-limited",
		"--agent", "harness=claude,model=sonnet",
		"--signed-in", "claude", "--check", "true", "--tools", "none")
	if code != 1 {
		t.Fatalf("init = %d\n%s\n%s", code, out, err)
	}
	if !strings.Contains(out+err, "marked as read only") || !strings.Contains(out+err, "write access") {
		t.Fatalf("missing the read-only diagnosis:\n%s\n%s", out, err)
	}
	if _, e := os.Stat(".brigada"); !os.IsNotExist(e) {
		t.Fatal("init wrote .brigada despite the refused push")
	}
	if _, e := os.Stat(state); !os.IsNotExist(e) {
		t.Fatal("init wrote state despite the refused push")
	}
}

func TestInitPushProbeGH007FailsInitBeforeWrite(t *testing.T) {
	importRepo(t)
	remote := initPushURL(t)
	probeHook(t, remote, "echo \"error: GH007: Your push would publish a private email address.\" >&2\nexit 1\n")
	fakeInitPATH(t, "claude", "codex")
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	code, out, err := initDo(t, state, "", "--yes", "--accept-limited",
		"--agent", "harness=claude,model=sonnet",
		"--signed-in", "claude", "--check", "true", "--tools", "none")
	if code != 1 || !strings.Contains(out+err, "GH007") || !strings.Contains(out+err, "noreply") {
		t.Fatalf("init = %d\n%s\n%s", code, out, err)
	}
	if _, e := os.Stat(".brigada"); !os.IsNotExist(e) {
		t.Fatal("init wrote .brigada despite the refused push")
	}
}

func TestInitPushProbeUnreachableContinues(t *testing.T) {
	binary := testsupport.BuildBrigada(t)
	old := initBinary
	initBinary = binary
	t.Cleanup(func() { initBinary = old })
	importRepo(t)
	// A push target that cannot be reached: init warns and continues, exiting 0.
	wireGit(t, ".", "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "absent.git"))
	fakeInitPATH(t, "claude", "codex")
	state := testsupport.ShortStateDir(t)
	initApplyCleanup(t, state)
	code, out, err := initDo(t, state, "", "--yes", "--accept-limited",
		"--agent", "harness=claude,model=sonnet",
		"--signed-in", "claude", "--check", "true", "--tools", "none")
	if code != 0 {
		t.Fatalf("init = %d\n%s\n%s", code, out, err)
	}
	if !strings.Contains(out, "WARNING:") || !strings.Contains(out, "push path could not be verified") {
		t.Fatalf("missing the unreachable warning:\n%s", out)
	}
}

func TestInitPushProbeFlagPushesNothing(t *testing.T) {
	importRepo(t)
	remote := initPushURL(t)
	fakeInitPATH(t, "claude", "codex")
	state := filepath.Join(testsupport.ShortStateDir(t), "absent")
	code, out, err := initDo(t, state, "", "--probe",
		"--agent", "harness=claude,model=sonnet",
		"--agent", "harness=codex,model=gpt-5",
		"--signed-in", "claude,codex", "--check", "true", "--tools", "none")
	if code != 0 {
		t.Fatalf("--probe = %d\n%s\n%s", code, out, err)
	}
	if out := wireGit(t, ".", "ls-remote", remote, initProbeRefPrefix+"*"); strings.TrimSpace(out) != "" {
		t.Fatalf("--probe pushed: %s", out)
	}
}

func TestInitParentAllPushesNothing(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	parent := t.TempDir()
	remote := t.TempDir()
	wireGit(t, remote, "init", "--bare", "-q")
	parentRepo(t, parent, "child", remote)
	t.Chdir(parent)
	fakeInitPATH(t)
	code, out, errout := runWithStdin(t, ctx, state, "", []string{"init", "--yes", "--all", "--foreman", "claude/sonnet"})
	if code != 0 {
		t.Fatalf("init --all = %d\n%s\n%s", code, out, errout)
	}
	if refs := wireGit(t, filepath.Join(parent, "child"), "ls-remote", "origin", initProbeRefPrefix+"*"); strings.TrimSpace(refs) != "" {
		t.Fatalf("init --all pushed to a child: %s", refs)
	}
}
