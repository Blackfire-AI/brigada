package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// runBudget is `brigada budget` (§4.3): the project's foreman wake
// budget — the daily bound on headless foreman turns — shown with
// today's use and the reset time, and raised with --wakes, which also
// re-evaluates the wake of anything the spent budget parked. The
// task-worker-minutes raise of the design (`budget <plan> +<minutes>`)
// is not built; this command carries the wake budget alone, so it takes
// no positional arguments.
func runBudget(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("budget",
		"brigada budget [--repo URL] [--wakes N] [--no-start]",
		"Show or set a project’s daily foreman wake budget.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		repo    string
		wakes   int
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the repository URL (default: the engine's only project, like ask)")
	fs.IntVar(&wakes, "wakes", 0, "set the project's daily foreman wake budget, 1 to "+strconv.Itoa(engine.MaxForemanWakesPerDay)+"; omitted = show it alone")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "budget takes no positional arguments, got %q; the wake budget is a flag: --wakes N", extra)
	}
	// An explicit --wakes is validated however it reads: 0 is not a
	// budget, and an unset flag — not named at all — is the plain show.
	wakesSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "wakes" {
			wakesSet = true
		}
	})
	if wakesSet {
		if err := engine.ValidateSettingValue(engine.SettingForemanWakesPerDay, strconv.Itoa(wakes)); err != nil {
			return usagef(stderr, "%v", err)
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
		info *engine.WakeBudgetInfo
		err  error
		set  bool
	)
	if wakesSet {
		if info, err = client.SetWakeBudget(ctx, repo, wakes); err != nil {
			return fail(stderr, err)
		}
		set = true
	} else if info, err = client.WakeBudget(ctx, repo); err != nil {
		return fail(stderr, err)
	}
	src := "default"
	if !info.Default {
		src = "set"
	}
	if set {
		fmt.Fprintf(stdout, "foreman wake budget set to %d per day\n", info.Budget)
	}
	fmt.Fprintf(stdout, "%s: foreman wake budget %d per day (%s), %d used today, resets %s\n",
		info.RepoURL, info.Budget, src, info.Wakes, info.ResetsAt)
	if info.Waiting > 0 {
		line := fmt.Sprintf("; %d events waiting", info.Waiting)
		if info.Wakes >= info.Budget {
			line += " — budget spent; raise it with `brigada budget --wakes N` or send `brigada ask`"
		}
		fmt.Fprintln(stdout, line)
	}
	return 0
}
