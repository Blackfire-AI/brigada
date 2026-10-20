package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/projecturl"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// capacityClient is what capacity asks of the API client: the snapshot,
// and the projects, to name a repo's project by its id. Tests stub it.
type capacityClient interface {
	Capacity(ctx context.Context, projectID int64) (engine.CapacitySnapshot, error)
	Projects(ctx context.Context) ([]engine.ProjectInfo, error)
}

// runCapacity is `brigada capacity` (§12.2): the agents with their tier,
// effort, cost, running jobs against their cap, and quota, then the cap
// layers with their cap, use and queue, as two tables. It shows the
// project --project names, else the project of the working directory's
// repository; outside a project it shows every project's.
func runCapacity(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("capacity",
		"brigada capacity [--project REPO-URL] [--no-start]",
		"Show agents' caps, quota and queues, and each cap layer's use.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		project string
		noStart bool
	)
	fs.StringVar(&project, "project", "", "the repository URL whose project to show (default: the working directory's repository)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "capacity takes no positional arguments, got %q", extra)
	}
	repo := ""
	if project != "" {
		canonical, err := projecturl.Canonical(project)
		if err != nil {
			return usagef(stderr, "--project: %v", err)
		}
		repo = canonical
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	return printCapacity(ctx, client, repo, stdout, stderr)
}

// printCapacity prints the capacity tables of the project repo names, or of
// the working directory's repository when repo is empty.
func printCapacity(ctx context.Context, client capacityClient, repo string, stdout, stderr io.Writer) int {
	projectID, code := capacityProjectID(ctx, client, repo, stderr)
	if code != 0 {
		return code
	}
	snap, err := client.Capacity(ctx, projectID)
	if err != nil {
		return fail(stderr, err)
	}
	style := term.New(detectCaps(stdout))
	fmt.Fprintln(stdout, api.CapacityTables(snap, time.Now(), style, style.Width()))
	return 0
}

// capacityProjectID names the project capacity shows, 0 for every project.
// A repo named explicitly must be a project the engine has. Without one, the
// working directory's origin names it; a directory outside any repository,
// or one whose origin is no project, shows every project's capacity.
func capacityProjectID(ctx context.Context, client capacityClient, repo string, stderr io.Writer) (int64, int) {
	id, err := lookupProjectID(ctx, client, repo)
	if err != nil {
		return 0, fail(stderr, err)
	}
	return id, 0
}

// lookupProjectID is capacityProjectID's answer as an error, for a caller
// that shows the failure itself.
func lookupProjectID(ctx context.Context, client capacityClient, repo string) (int64, error) {
	explicit := repo != ""
	if !explicit {
		dir, err := os.Getwd()
		if err != nil {
			return 0, fmt.Errorf("find the working directory: %w", err)
		}
		origin, err := gitOriginURL(dir)
		if err != nil {
			return 0, nil
		}
		if repo, err = projecturl.Canonical(origin); err != nil {
			return 0, nil
		}
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range projects {
		if p.RepoURL == repo {
			return p.ProjectID, nil
		}
	}
	if explicit {
		return 0, fmt.Errorf("no active project for repo %s", engine.RedactRepoURL(repo))
	}
	return 0, nil
}
