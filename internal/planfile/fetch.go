package planfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Fetched is what Fetch read from a plans branch: the plans whose files
// parsed, and a line for each file that did not — recover rebuilds what
// it can and says what it could not.
type Fetched struct {
	Plans   []Plan
	Skipped []string
}

// Fetch reads every plan file on the target branch, the reverse of Push:
// it fetches the branch into a scratch repository and parses each
// .brigada/plans/*.yaml with the strict reader. A remote without the
// branch holds no plans, which is not an error. A file whose name is not
// <id>.yaml for the id it holds is skipped, never trusted.
func Fetch(ctx context.Context, t Target) (Fetched, error) {
	if t.URL == "" {
		return Fetched{}, errors.New("no remote URL to read the plan files from")
	}
	if err := checkBranch(ctx, t.Branch); err != nil {
		return Fetched{}, err
	}
	dir, err := os.MkdirTemp("", "brigada-planfile-")
	if err != nil {
		return Fetched{}, fmt.Errorf("scratch repository: %w", err)
	}
	defer os.RemoveAll(dir)
	g := func(args ...string) (string, error) { return git(ctx, dir, args...) }

	if _, err := g("init", "-q"); err != nil {
		return Fetched{}, err
	}
	heads, err := g("ls-remote", "--heads", t.URL, "refs/heads/"+t.Branch)
	if err != nil {
		return Fetched{}, err
	}
	if strings.TrimSpace(heads) == "" {
		return Fetched{}, nil
	}
	if _, err := g("fetch", "-q", "--depth=1", t.URL, "+refs/heads/"+t.Branch+":refs/remotes/mirror/"+t.Branch); err != nil {
		return Fetched{}, err
	}
	if _, err := g("checkout", "-q", "-B", t.Branch, "refs/remotes/mirror/"+t.Branch); err != nil {
		return Fetched{}, err
	}
	files, err := filepath.Glob(filepath.Join(dir, filepath.FromSlash(Dir), "*.yaml"))
	if err != nil {
		return Fetched{}, err
	}
	sort.Strings(files)
	var out Fetched
	for _, file := range files {
		name := filepath.Base(file)
		data, err := os.ReadFile(file)
		if err != nil {
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		p, err := Parse(data)
		if err != nil {
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if filepath.Base(Path(p.ID)) != name {
			out.Skipped = append(out.Skipped, fmt.Sprintf("%s: the file holds plan %d", name, p.ID))
			continue
		}
		out.Plans = append(out.Plans, p)
	}
	return out, nil
}
