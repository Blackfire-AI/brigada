package main

import (
	"context"
	"fmt"
	"io"
)

// runProject is the project command's dispatch: `project foreman`, the
// foreman's harness and model (§4.1) — the user's choice of which CLI
// runs the foreman and which model it runs on.
func runProject(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usagef(stderr, "usage: brigada project foreman --repo URL [--harness H --model M]")
	}
	switch args[0] {
	case "foreman":
		return runProjectForeman(ctx, args[1:], stdout, stderr)
	default:
		return usagef(stderr, "unknown project subcommand %q; want foreman", args[0])
	}
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
	fs.StringVar(&repo, "repo", "", "the repository URL (product §5 rule 6)")
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
		return usagef(stderr, "project foreman needs --repo URL")
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
