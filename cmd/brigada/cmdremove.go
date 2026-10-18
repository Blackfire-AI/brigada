package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

var removeStdin io.Reader = os.Stdin

// Tests can exercise the real authenticated API handler without a listener.
var connectRemoval = connect

func runRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("remove",
		"brigada remove [--yes] [--cancel] [--delete-config]",
		"Remove brigada from this repository, keeping its project history.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	yes := fs.Bool("yes", false, "apply the displayed plan and stop an unused runner without prompting")
	cancel := fs.Bool("cancel", false, "cancel live jobs and unfinished plans when removing the project")
	deleteConfig := fs.Bool("delete-config", false, "also delete this repository's .brigada/project.yaml")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if len(fs.Args()) != 0 {
		return usagef(stderr, "remove takes no positional arguments")
	}
	dir, err := os.Getwd()
	if err != nil {
		return failRemoval(stderr, err)
	}
	root, err := gitRoot(dir)
	if err != nil {
		return failRemoval(stderr, errors.New("run brigada remove inside the repository"))
	}
	origin, err := gitOriginURL(root)
	if err != nil {
		return failRemoval(stderr, err)
	}
	repo, err := engine.Canonicalize(origin)
	if err != nil {
		return failRemoval(stderr, err)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connectRemoval(ctx, rt, "client", false, stderr)
	if code != 0 {
		return code
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		return failRemoval(stderr, err)
	}
	var project int64
	others := 0
	for _, p := range projects {
		if p.RepoURL == repo {
			project = p.ProjectID
		} else {
			others++
		}
	}
	live := 0
	if project != 0 {
		st, err := client.ProjectReader(project).Status(ctx)
		if err != nil {
			return failRemoval(stderr, err)
		}
		for _, state := range []string{"queued", "leased", "running"} {
			live += st.Jobs[state]
		}
		for _, p := range st.Plans {
			if p.State == "draft" || p.State == "pending_approval" || p.State == "active" {
				live++
			}
		}
		for _, item := range st.NeedsYou {
			if item.Kind == "plan" && item.State == "pending_approval" {
				live++
			}
		}
	}
	_, running, err := runnerstate.Running(runnerStateDir(rt))
	if err != nil {
		return failRemoval(stderr, err)
	}
	files := planRemoval(root, *deleteConfig)
	printRemovalPlan(stdout, repo, files, live, *cancel, others, running)
	printRemovalRemote(ctx, stdout, root)
	if _, err := exec.LookPath("codex"); err == nil {
		fmt.Fprintln(stdout, "\n  Codex (if you ran it): codex mcp remove brigada-foreman")
	}
	if live > 0 && !*cancel {
		return failRemoval(stderr, errors.New("live work remains; wait for it to finish or pass --cancel"))
	}
	input := bufio.NewReader(removeStdin)
	confirm := func(prompt string, defaultYes bool) (bool, error) {
		if *yes {
			return true, nil
		}
		fmt.Fprint(stdout, prompt)
		line, err := input.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		fmt.Fprintln(stdout)
		answer := strings.ToLower(strings.TrimSpace(line))
		// EOF never silently authorizes a default-yes operation.
		return answer == "y" || answer == "yes" || (defaultYes && answer == "" && err == nil), nil
	}
	apply, err := confirm("\nRemove brigada from this repository? [y/N]: ", false)
	if err != nil {
		return failRemoval(stderr, err)
	}
	if !apply {
		fmt.Fprintln(stdout, "Nothing removed.")
		return 0
	}
	if err := checkRemoval(files); err != nil {
		return failRemoval(stderr, err)
	}
	// Archive is the authoritative, atomic busy guard, including work that
	// arrived while the user was reviewing the plan. No file has changed yet.
	_, err = client.ArchiveProject(ctx, origin, *cancel)
	var se *api.StatusError
	switch {
	case errors.As(err, &se) && se.Status == http.StatusConflict:
		return failRemoval(stderr, errors.New("live work remains; wait for it to finish or pass --cancel"))
	case errors.As(err, &se) && se.Status == http.StatusNotFound:
		// Unregistered repositories still have local wiring to remove.
	case err != nil:
		return failRemoval(stderr, err)
	}
	if err := applyRemoval(files); err != nil {
		return failRemoval(stderr, err)
	}
	fmt.Fprintln(stdout, "brigada removed; project history kept.")
	// Refresh after archive: another project may have registered meanwhile.
	projects, err = client.Projects(ctx)
	if err != nil {
		return failRemoval(stderr, err)
	}
	if len(projects) != 0 {
		fmt.Fprintln(stdout, "Other projects use the runner; it keeps running.")
		return 0
	}
	_, running, err = runnerstate.Running(runnerStateDir(rt))
	if err != nil {
		return failRemoval(stderr, err)
	}
	if !running {
		return 0
	}
	stop, err := confirm("stop the runner? [Y/n] ", true)
	if err != nil {
		return failRemoval(stderr, err)
	}
	if !stop {
		return 0
	}
	stopArgs := []string{"--state-dir", rt.stateDir}
	if rt.explicitCfg {
		stopArgs = append(stopArgs, "--config", rt.configPath)
	}
	return runStop(ctx, stopArgs, stdout, stderr)
}

func printRemovalPlan(w io.Writer, repo string, files []wireFile, live int, cancel bool, others int, running bool) {
	printInitWrapped(w, "brigada remove: ", "  ", repo)
	fmt.Fprintln(w, "\n  In this repository (only what brigada added):")
	for _, f := range files {
		if !f.exists && !f.changed() && f.err == nil {
			continue
		}
		var action string
		switch {
		case f.err != nil:
			action = "left alone: " + f.err.Error()
		case f.rel == engine.ProjectConfigPath && !f.changed():
			action = "keep (yours; delete it yourself or pass --delete-config)"
		case !f.changed():
			continue
		case f.rel == wiringMCPFile:
			action = "remove the \"brigada\" server"
		case f.rel == wiringSkillFile:
			action = "delete (brigada's own file)"
		case f.rel == wiringSettingsFile:
			action = "remove the Stop hook (brigada hold yield)"
		case f.rel == wiringSharedSettingsFile:
			action = "remove the legacy brigada hooks"
		case f.rel == engine.ProjectConfigPath:
			action = "delete (--delete-config)"
		default:
			action = "remove /" + wiringSettingsFile
		}
		printInitWrapped(w, "    ", "      ", f.rel)
		printInitWrapped(w, "      ", "      ", action)
		if f.changed() && f.next == nil && f.rel != wiringSkillFile && f.rel != engine.ProjectConfigPath {
			fmt.Fprintln(w, "      (file deleted: nothing else in it)")
		}
	}
	fmt.Fprintln(w, "  In brigada:")
	printInitWrapped(w, "    ", "      ", "the project: no longer registered; plans, tasks and stats are kept as history (brigada history --all still shows them)")
	if live == 0 {
		fmt.Fprintln(w, "    live work: none")
	} else if cancel {
		fmt.Fprintln(w, "    live work: cancel live jobs and unfinished plans (--cancel)")
	} else {
		fmt.Fprintln(w, "    live work: wait for it to finish or pass --cancel")
	}
	switch {
	case others > 0:
		fmt.Fprintln(w, "    the runner: other projects use it; it keeps running")
	case running:
		fmt.Fprintln(w, "    the runner: last project; offer to stop it after removal")
	default:
		fmt.Fprintln(w, "    the runner: none running")
	}
}

func printRemovalRemote(ctx context.Context, w io.Writer, root string) {
	fmt.Fprintln(w, "\n  On the remote (not touched; remove them yourself if you like):")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-remote", "origin", "refs/heads/brigada/*")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(w, "    could not be checked (git ls-remote failed)")
		return
	}
	var branches []string
	tasks, plans := 0, false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		branch := strings.TrimPrefix(fields[1], "refs/heads/")
		switch {
		case branch == "brigada/plans":
			plans = true
		case strings.HasPrefix(branch, "brigada/task-"):
			tasks++
		default:
			continue
		}
		branches = append(branches, shellQuote(branch))
	}
	text := fmt.Sprintf("%d brigada/task-* branches", tasks)
	if plans {
		text += ", brigada/plans"
	}
	printInitWrapped(w, "    ", "      ", text)
	if len(branches) > 0 {
		// One command per branch keeps the printed commands copyable and short.
		for _, branch := range branches {
			printInitWrapped(w, "    ", "      ", "git push origin --delete "+branch)
		}
	}
}

// Removal plans and diagnostics share the terminal's 79-column budget.
func failRemoval(w io.Writer, err error) int {
	printInitWrapped(w, "brigada: ", "  ", err.Error())
	return 1
}
