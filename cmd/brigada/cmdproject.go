package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// runProject is the project command's dispatch: `project foreman`, the
// foreman's harness and model (§4.1) — the user's choice of which CLI
// runs the foreman and which model it runs on — and `project list`, every
// project the engine knows.
func runProject(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usagef(stderr, "usage: brigada project foreman [--repo URL] [--harness H --model M] | brigada project list")
	}
	switch args[0] {
	case "foreman":
		return runProjectForeman(ctx, args[1:], stdout, stderr)
	case "list":
		return runProjectList(ctx, args[1:], stdout, stderr)
	default:
		return usagef(stderr, "unknown project subcommand %q; want foreman or list", args[0])
	}
}

// runProjectList prints one line per project: the repo URL, the foreman's
// harness/model ("-" while none is chosen), the active plans and what
// waits on the user.
func runProjectList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project list")
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code := common.parse(stderr, args); code != 0 {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "project list takes no positional arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if len(projects) == 0 {
		fmt.Fprintln(stdout, "no projects; create one: brigada project foreman --repo <url> --harness <harness> --model <model>")
		return 0
	}
	for _, p := range projects {
		foreman := "-"
		if p.Harness != "" && p.Model != "" {
			foreman = p.Harness + "/" + p.Model
		}
		fmt.Fprintf(stdout, "%s  foreman=%s  active_plans=%d  needs_you=%d\n", p.RepoURL, foreman, p.ActivePlans, p.NeedsYou)
	}
	return 0
}

// knownProjectsHint names the projects the engine has, for the error of a
// command that could not map its repo to one: the URLs to pass to --repo.
// Empty when the engine cannot be asked, so the original error stands
// alone.
func knownProjectsHint(ctx context.Context, client *api.Client) string {
	projects, err := client.Projects(ctx)
	if err != nil {
		return ""
	}
	if len(projects) == 0 {
		return "the engine has no projects yet"
	}
	urls := make([]string, len(projects))
	for i, p := range projects {
		urls[i] = p.RepoURL
	}
	return "known projects: " + strings.Join(urls, ", ")
}

// isNotFound reports whether err is the API's 404.
func isNotFound(err error) bool {
	var se *api.StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// runProjectForeman shows or sets the foreman's harness and model for a
// repo (§4.1): with --harness and --model it chooses them — the harness
// must resume headless, which the engine refuses otherwise — and prints
// the stored choice; with neither it just prints what is stored, empty
// fields and all.
func runProjectForeman(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project foreman")
	common := addCommon(fs)
	var (
		repo    string
		harness string
		model   string
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the repository URL (product §5 rule 6); empty = the origin of the working directory")
	fs.StringVar(&harness, "harness", "", "the foreman's harness: a CLI that resumes headless (§4.1); empty = show the choice")
	fs.StringVar(&model, "model", "", "the foreman's model; goes with --harness")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code := common.parse(stderr, args); code != 0 {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "project foreman takes no positional arguments, got %q", extra)
	}
	if repo == "" {
		// Like attach: inside a repo, the project is the one its origin names.
		dir, err := os.Getwd()
		if err != nil {
			return fail(stderr, fmt.Errorf("find the working directory: %w", err))
		}
		origin, err := gitOriginURL(dir)
		if err != nil {
			return usagef(stderr, "project foreman needs --repo URL outside a repository (%v)", err)
		}
		repo = origin
	}
	if (harness == "") != (model == "") {
		return usagef(stderr, "project foreman sets --harness and --model together; with neither it shows the choice")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	if harness == "" {
		setting, err := client.ProjectForeman(ctx, repo)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "harness=%s model=%s\n", setting.Harness, setting.Model)
		return 0
	}
	setting, err := client.SetProjectForeman(ctx, repo, harness, model)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "foreman set: harness=%s model=%s\n", setting.Harness, setting.Model)
	return 0
}
