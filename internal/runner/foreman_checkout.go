package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/foreman"
)

// PrepareForemanCheckout refreshes the project's reading copy for both a
// headless turn and attach. The session stays in scratch: moving its cwd
// would lose the harness's session identity. Only this preparation writes
// the checkout; the foreman must receive read-only access from its harness.
func PrepareForemanCheckout(ctx context.Context, scratch, repoURL, branch string, gitEnv []string) (foreman.Checkout, error) {
	if repoURL == "" || branch == "" {
		return foreman.Checkout{}, fmt.Errorf("the foreman requires a project repo URL and default branch")
	}
	dir, err := filepath.Abs(filepath.Join(scratch, "repo"))
	if err != nil {
		return foreman.Checkout{}, err
	}
	w := &workspace{gitEnv: gitEnv}
	git := func(args ...string) (string, error) {
		out, err := w.git(ctx, append([]string{"-c", "core.hooksPath=" + os.DevNull}, args...)...)
		if err != nil {
			return "", trimCmdError(out, err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := git("check-ref-format", "--branch", branch); err != nil {
		return foreman.Checkout{}, fmt.Errorf("invalid default branch %q: %w", branch, err)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// Clone aside: a failed clone never leaves a partial repo for a retry.
		tmp, err := os.MkdirTemp(scratch, "repo-clone-")
		if err != nil {
			return foreman.Checkout{}, err
		}
		defer os.RemoveAll(tmp)
		if _, err := git("clone", "--no-checkout", "--", repoURL, tmp); err != nil {
			return foreman.Checkout{}, fmt.Errorf("clone %s: %w", redactURL(repoURL), err)
		}
		if err := os.Rename(tmp, dir); err != nil {
			return foreman.Checkout{}, err
		}
	} else if err != nil {
		return foreman.Checkout{}, err
	}
	// The URL belongs to the project. Disable pushes as a further guard;
	// harness permissions, rather than git configuration, prevent edits.
	for _, kv := range [][2]string{
		{"remote.origin.url", repoURL},
		{"remote.origin.pushurl", "disabled://foreman-read-only"},
	} {
		if _, err := git("-C", dir, "config", kv[0], kv[1]); err != nil {
			return foreman.Checkout{}, err
		}
	}
	remoteRef := "refs/remotes/origin/" + branch
	if _, err := git("-C", dir, "fetch", "--no-tags", "--prune", "origin", "+refs/heads/"+branch+":"+remoteRef); err != nil {
		return foreman.Checkout{}, fmt.Errorf("fetch %s branch %s: %w", redactURL(repoURL), branch, err)
	}
	if _, err := git("-C", dir, "checkout", "--force", "-B", branch, remoteRef, "--"); err != nil {
		return foreman.Checkout{}, fmt.Errorf("reset the foreman checkout to %s: %w", branch, err)
	}
	if _, err := git("-C", dir, "clean", "-ffdx"); err != nil {
		return foreman.Checkout{}, err
	}
	commit, err := git("-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return foreman.Checkout{}, err
	}
	return foreman.Checkout{Path: dir, Branch: branch, Commit: commit}, nil
}
