package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// runProject is the project command's dispatch: `project foreman`, the
// foreman's harness and model (§4.1) — the user's choice of which CLI
// runs the foreman and which model it runs on — and `project list`, every
// project the engine knows.
func runProject(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return usagef(stderr, "usage: brigada project foreman|list|set|show [--repo URL] ...")
	}
	switch args[0] {
	case "foreman":
		return runProjectForeman(ctx, args[1:], stdout, stderr)
	case "list":
		return runProjectList(ctx, args[1:], stdout, stderr)
	case "set":
		return runProjectSettings(ctx, "set", args[1:], stdout, stderr)
	case "show":
		return runProjectSettings(ctx, "show", args[1:], stdout, stderr)
	default:
		return usagef(stderr, "unknown project subcommand %q; want foreman, list, set or show", args[0])
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

// runProjectSettings is `project set` and `project show` (§6.3): the
// approval limit's three settings. set takes key=value pairs after the
// flags and prints the settings as they now stand; show prints every key
// with its effective value and whether it is the default. --repo
// defaults to the working directory's origin. Values are validated by
// the engine's own parser before the engine is contacted, so a value
// accepted here is one the limit understands.
func runProjectSettings(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project " + verb)
	common := addCommon(fs)
	var (
		repo    string
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the repository URL (default: the working directory's origin)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code := common.parse(stderr, args); code != 0 {
		return code
	}
	validKeys := strings.Join(engine.SettingKeys, ", ")
	values := map[string]string{}
	pairs := fs.Args()
	if verb == "show" && len(pairs) > 0 {
		return usagef(stderr, "project show takes no positional arguments, got %q", pairs)
	}
	if verb == "set" && len(pairs) == 0 {
		return usagef(stderr, "usage: brigada project set [--repo URL] key=value ...; valid keys: %s", validKeys)
	}
	for _, p := range pairs {
		key, value, ok := strings.Cut(p, "=")
		if !ok {
			return usagef(stderr, "project set wants key=value, got %q; valid keys: %s", p, validKeys)
		}
		if _, dup := values[key]; dup {
			return usagef(stderr, "project set names %s twice", key)
		}
		if err := engine.ValidateSettingValue(key, value); err != nil {
			return usagef(stderr, "%v", err)
		}
		values[key] = value
	}
	if repo == "" {
		dir, err := os.Getwd()
		if err != nil {
			return fail(stderr, fmt.Errorf("find the working directory: %w", err))
		}
		if repo, err = gitOriginURL(dir); err != nil {
			return fail(stderr, err)
		}
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	var (
		got engine.ProjectSettings
		err error
	)
	if verb == "set" {
		got, err = client.SetProjectSettings(ctx, repo, values)
	} else {
		got, err = client.ProjectSettings(ctx, repo)
		var se *api.StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return fail(stderr, fmt.Errorf("no project for repo %s; set one of its settings or choose its foreman first", repo))
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	for _, s := range got.Settings {
		state := "set"
		if s.Default {
			state = "default"
		}
		fmt.Fprintf(stdout, "%s=%s (%s)\n", s.Key, s.Value, state)
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
		effort  string
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the repository URL (product §5 rule 6); empty = the origin of the working directory")
	fs.StringVar(&harness, "harness", "", "the foreman's harness: a CLI that resumes headless (§4.1); empty = show the choice")
	fs.StringVar(&model, "model", "", "the foreman's model; goes with --harness")
	fs.StringVar(&effort, "effort", "", "the foreman's reasoning effort, validated per harness (claude: low|medium|high|xhigh|max); goes with --harness; empty = the CLI's default")
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
	if harness == "" && effort != "" {
		return usagef(stderr, "project foreman sets --effort with --harness and --model; with none of them it shows the choice")
	}
	if err := adapter.ValidateEffort(harness, effort); err != nil {
		return usagef(stderr, "--effort: %v", err)
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
		fmt.Fprintf(stdout, "harness=%s model=%s%s\n", setting.Harness, setting.Model, effortField(setting.Effort))
		return 0
	}
	setting, err := client.SetProjectForeman(ctx, repo, harness, model, effort)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "foreman set: harness=%s model=%s%s\n", setting.Harness, setting.Model, effortField(setting.Effort))
	if setting.Unserved {
		// The choice stands, but a foreman turn runs only on an agent that
		// offers exactly this pair: until one is registered the turns wait.
		fmt.Fprintf(stderr, "warning: no registered runner agent offers %[1]s/%[2]s; foreman turns will wait until one does. Start a runner (another one, if yours already offers %[1]s) with --agent harness=%[1]s,model=%[2]s\n",
			setting.Harness, setting.Model)
	}
	return 0
}

// agentName is an agent's display name, harness/model, with @effort only
// where an effort is set: claude/opus@high.
func agentName(harness, model, effort string) string {
	name := harness + "/" + model
	if effort != "" {
		name += "@" + effort
	}
	return name
}

// effortField is the " effort=<level>" suffix of a printed foreman choice;
// empty when no effort is set, so an unset effort prints as before.
func effortField(effort string) string {
	if effort == "" {
		return ""
	}
	return " effort=" + effort
}
