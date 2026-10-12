// Command guard scans a repository for material that must not be
// published: private and special IPv4 literals, private-network hostnames
// and issued credential shapes. It is run in CI as
//
//	go run ./internal/tools/guard
//
// and works from any directory inside the repository. Every file tracked
// by git is scanned; content containing a NUL byte is treated as binary
// and skipped. Each finding is printed as `path:line: rule: excerpt` and
// the exit code is 1. Justified exceptions live in `.guard-allow` at the
// repository root, one `path:regexp` per line.
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// allowFile is the repository-root allowlist, the one tracked file the
// guard does not scan: its entries quote the very shapes they permit.
const allowFile = ".guard-allow"

func main() {
	os.Exit(run(os.Stdout, os.Stderr))
}

// run scans the repository and returns the process exit code: 1 for any
// finding or scan error, 0 when the tree is clean.
func run(stdout, stderr io.Writer) int {
	root, err := repoRoot()
	if err != nil {
		return fail(stderr, err)
	}
	exs, err := loadAllowlist(filepath.Join(root, allowFile))
	if err != nil {
		return fail(stderr, err)
	}
	paths, err := trackedFiles(root)
	if err != nil {
		return fail(stderr, err)
	}
	found := 0
	for _, path := range paths {
		if path == allowFile {
			continue
		}
		full := filepath.Join(root, path)
		if info, err := os.Stat(full); err == nil && info.IsDir() {
			continue // a submodule or similar nested repository
		}
		data, err := os.ReadFile(full)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // deleted from the working tree but still tracked
			}
			return fail(stderr, err)
		}
		for _, f := range scanFile(path, data, exs) {
			fmt.Fprintln(stdout, f)
			found++
		}
	}
	if found > 0 {
		fmt.Fprintf(stderr, "guard: %d findings\n", found)
		return 1
	}
	return 0
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "guard: %v\n", err)
	return 1
}

// repoRoot is the top of the git work tree, so the guard works from any
// directory inside the repository.
func repoRoot() (string, error) {
	out, err := gitOutput("", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// trackedFiles lists the files git tracks, relative to the repository root
// and sorted so the output is stable.
func trackedFiles(root string) ([]string, error) {
	out, err := gitOutput(root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(string(out), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func loadAllowlist(path string) ([]exception, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read allowlist: %w", err)
	}
	return parseAllowlist(string(data))
}
