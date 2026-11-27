package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/projecturl"
)

// initProbeRefPrefix is the namespace init's push probe uses. It sits
// outside refs/heads, so the probe never touches the repository's branches,
// and a delete push removes it again.
const initProbeRefPrefix = "refs/brigada/probe/"

// initProbeOutcome classifies a probe push.
type initProbeOutcome int

const (
	// probeOutcomeOK: the push (and its delete) worked.
	probeOutcomeOK initProbeOutcome = iota
	// probeOutcomeRefused: the remote answered and refused the push, its own
	// words in hand — a read-only key, a GH007/private-email rejection, a
	// "Permission to ... denied", a repository not found, a pre-receive
	// hook, a credential it turned away. Init fails before any write; a
	// later brigada doctor reports the same outcome.
	probeOutcomeRefused
	// probeOutcomeUnreachable: the remote could not be reached at all —
	// git's own transport error (DNS, connection refused or timed out, a
	// TLS handshake), with no response from the remote. Init warns and
	// continues, because it may legitimately run offline; a later brigada
	// doctor reports the same outcome.
	probeOutcomeUnreachable
)

// initProbeResult is what a push probe reports to its callers.
type initProbeResult struct {
	outcome initProbeOutcome
	// words are the remote's own words for a refusal, or git's transport
	// error for an unreachable remote, verbatim but for a credential in a
	// URL, which is redacted; empty when the push worked.
	words string
	// ref is the full ref the probe pushed (and normally deleted), set when
	// the push succeeded.
	ref string
	// deleteErr is a failed delete push: the ref survives, and the caller
	// warns with the ref's name and the command to delete it.
	deleteErr error
}

// runInitPushProbe verifies the push path for remote without touching the
// repository's HEAD, index, working tree or refs/heads. It builds a scratch
// commit from HEAD's tree — or the empty tree when HEAD is unborn, a fresh
// repository with a remote but no commits — under the machine's git identity
// (user.name/user.email), pushes it as refs/brigada/probe/<hex> and deletes
// it again with a second push.
//
// The probe is remote-agnostic — push a ref, delete it — so any remote
// catches a read-only ambient credential; only initProbeFix names GitHub's
// signatures. A failed push is classified, never returned as an error: see
// probePushRefused. A failed delete leaves the ref on the remote and is
// reported in deleteErr; nothing later (no init, no doctor) reconciles
// probe refs.
//
// The commit carries the machine's identity and the push uses the ambient
// credential environment, so a passing probe verifies only that path: the
// plan-file mirror commits as brigada <brigada@brigada.invalid>, the landing
// clone may keep the host identity or fall back to brigada runner
// <runner@brigada.invalid>, and the runner may push with GitEnv credentials
// distinct from init's ambient ones. A rejection of those other identities
// or credentials is not caught here.
func runInitPushProbe(ctx context.Context, root, remote string) (initProbeResult, error) {
	name, err := probeGitIdentity(ctx, root, "user.name")
	if err != nil {
		return initProbeResult{}, err
	}
	email, err := probeGitIdentity(ctx, root, "user.email")
	if err != nil {
		return initProbeResult{}, err
	}

	// The scratch commit is built in a temporary index, so the repository's
	// own index is never read or written.
	dir, err := os.MkdirTemp("", "brigada-init-probe-")
	if err != nil {
		return initProbeResult{}, fmt.Errorf("push probe: %w", err)
	}
	defer os.RemoveAll(dir)
	indexPath := filepath.Join(dir, "index")

	// HEAD's tree, or the empty tree when HEAD is unborn (rev-parse HEAD
	// then fails; the probe still runs against the empty tree).
	treeRef := "HEAD"
	if _, _, err := probeGit(ctx, root, nil, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		empty, _, err := probeGit(ctx, root, nil, "hash-object", "-t", "tree", os.DevNull)
		if err != nil {
			return initProbeResult{}, fmt.Errorf("push probe: empty tree: %w", err)
		}
		treeRef = strings.TrimSpace(empty)
	}

	indexEnv := []string{"GIT_INDEX_FILE=" + indexPath}
	if _, _, err := probeGit(ctx, root, indexEnv, "read-tree", treeRef); err != nil {
		return initProbeResult{}, fmt.Errorf("push probe: read tree: %w", err)
	}
	tree, _, err := probeGit(ctx, root, indexEnv, "write-tree")
	if err != nil {
		return initProbeResult{}, fmt.Errorf("push probe: write tree: %w", err)
	}

	identityEnv := []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email,
	}
	commit, _, err := probeGit(ctx, root, identityEnv, "commit-tree", strings.TrimSpace(tree), "-m", "brigada init push probe")
	if err != nil {
		return initProbeResult{}, fmt.Errorf("push probe: commit: %w", err)
	}

	suffix, err := initProbeSuffix()
	if err != nil {
		return initProbeResult{}, err
	}
	ref := initProbeRefPrefix + suffix

	_, stderr, pushErr := probeGit(ctx, root, nil, "push", remote, strings.TrimSpace(commit)+":"+ref)
	if pushErr != nil {
		res := initProbeResult{words: projecturl.RedactText(strings.TrimSpace(stderr))}
		if probePushRefused(stderr) {
			res.outcome = probeOutcomeRefused
		} else {
			res.outcome = probeOutcomeUnreachable
		}
		return res, nil
	}

	res := initProbeResult{outcome: probeOutcomeOK, ref: ref}
	if _, deleteStderr, err := probeGit(ctx, root, nil, "push", remote, "--delete", ref); err != nil {
		res.deleteErr = fmt.Errorf("%v: %s", err, projecturl.RedactText(strings.TrimSpace(deleteStderr)))
	}
	return res, nil
}

// probeGitIdentity reads one git identity setting as git would for a commit
// in root, and refuses an unset (or empty) one, naming the fix.
func probeGitIdentity(ctx context.Context, root, key string) (string, error) {
	out, _, err := probeGit(ctx, root, nil, "config", "--get", key)
	if err != nil || strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("push probe needs a git identity: %s is not set; set it before init (for example, git config --global %s \"...\")", key, key)
	}
	return strings.TrimSpace(out), nil
}

// probeGit runs one git command in root with terminal prompts off — a remote
// that wants credentials the probe does not have fails instead of hanging —
// and returns stdout and stderr separately, so the caller can classify a
// failed push from the remote's own words. The credential environment is
// otherwise the ambient one, as the runner's own pushes keep it: an askpass
// program or a credential helper the user configured still answers. Git's
// own messages are pinned to English (LC_ALL=C) so the classification does
// not depend on the user's locale; the remote's words arrive verbatim
// either way.
func probeGit(ctx context.Context, root string, env []string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), append([]string{
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
	}, env...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// initProbeSuffix is a random hex name for the probe's ref.
func initProbeSuffix() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("push probe: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// probeRefusalSignatures mark a failed push the remote itself answered, as
// opposed to one that never reached it. A `remote:` or `ERROR:` line is the
// remote speaking (GitHub's read-only key, GH007, "Permission to ... denied",
// repository not found, a hook's output); the others are git or ssh
// relaying the remote's answer: a ref the remote rejected without a word,
// and a credential it turned away.
var probeRefusalSignatures = []string{
	"[remote rejected]",
	"Permission denied",
	"Authentication failed",
	"The requested URL returned error",
}

// probePushRefused sorts a failed probe push in two. It reports true when
// the remote answered and refused — stderr carries a `remote:` or `ERROR:`
// line or one of probeRefusalSignatures — which fails init before any
// write. Everything else is the remote not reached at all: git's own
// transport error (DNS resolution failure, connection refused or timed out,
// a TLS handshake failure, a path that is no repository), which init warns
// about and survives, because it may legitimately run offline.
func probePushRefused(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "remote:") || strings.HasPrefix(line, "ERROR:") {
			return true
		}
		for _, sig := range probeRefusalSignatures {
			if strings.Contains(line, sig) {
				return true
			}
		}
	}
	return false
}

// initProbeFix is the diagnosis table: it names the fix for a refused push
// whose remote words carry a known signature, and returns "" when init has
// no named fix and the remote's verbatim words stand alone. The signatures
// are GitHub's; the probe itself is remote-agnostic.
func initProbeFix(words string) string {
	switch {
	case strings.Contains(words, "marked as read only"):
		return "the key or token lacks write access"
	case strings.Contains(words, "GH007") || strings.Contains(words, "private email"):
		return "set your noreply user.email (or an includeIf per remote), then retry"
	case strings.Contains(words, "Permission to "):
		return "the credential's scope does not include this repository"
	}
	return ""
}
