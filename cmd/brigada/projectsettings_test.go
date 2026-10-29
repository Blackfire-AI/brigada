package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// startSettingsEngine runs a real engine in-process and returns its
// state directory.
func startSettingsEngine(t *testing.T) (context.Context, string) {
	t.Helper()
	state := testsupport.ShortStateDir(t)
	seedMigratedStore(t, lifecycle.PathsOf(state).Store)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lifecycle.Run(ctx, lifecycle.Config{StateDir: state, Idle: time.Minute}) }()
	t.Cleanup(func() {
		cancel()
		// Run returns only after its plan-file mirror's in-flight git
		// push has ended, so waiting here keeps that git clear of the
		// temp dirs removed after this cleanup. The ceiling is loose:
		// a loaded machine takes its time, and a stuck engine is still
		// reported.
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the engine stopped with an error: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the engine did not stop")
		}
	})
	socket := lifecycle.PathsOf(state).Socket
	deadline := time.Now().Add(10 * time.Second)
	for !lifecycle.EngineRunning(ctx, socket) {
		if time.Now().After(deadline) {
			t.Fatal("the engine did not answer in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ctx, state
}

// tailDefaults are the environment step's settings (§5.7, §5.8), the
// foreman wake budget (§4.3) and the project instructions (§5.9) at
// their defaults, the last lines of every settings print.
const tailDefaults = "setup=none (default)\n" + "setup_cache_key_files=none (default)\n" + "setup_timeout_minutes=15 (default)\n" +
	"extra_repos=none (default)\n" + "foreman_rotate_tokens=400000 (default)\n" + "foreman_wakes_per_day=20 (default)\n" + "plan_files=on (default)\n" + "instructions=none (default)\n"

// gateDefaults are the gates and land rules (§6.3) and the plan review
// (§6.2) at their defaults, printed between the approval keys and
// default_check.
const gateDefaults = "gate_plan=auto (default)\n" + "gate_land=auto (default)\n" + "gate_push=auto (default)\n" +
	"plan_review=true (default)\n" + "plan_review.tier=standard (default)\n" + "plan_review.effort=medium (default)\n" +
	"approval_tiers=none (default)\n" + "approval_restarted=false (default)\n" +
	"land_target=main (default)\n" + "after_land=none (default)\n" + "land_ready=none (default)\n"

// guidanceDefaults is the per-role block and skills line `project show`
// prints after the settings (§5.9) for a project that configured none of
// them: every role on the shipped default, generated from the defaults
// version, unedited.
const guidanceDefaults = "instructions.foreman: not configured, file none, 3998 bytes, sha256 a10014040f4683de74edab9bbc1ccce279b125d6d708521bb7572870f235caff, generated from 1.0.0, edited false\n" +
	"instructions.plan_review: not configured, file none, 797 bytes, sha256 5e40bd1559a16f947c20d32c59f385b3562fd4f332daaaa8038c532d73c9a4a3, generated from 1.0.0, edited false\n" +
	"instructions.worker: not configured, file none, 922 bytes, sha256 5df16b58db5a42599c40836cb9cb9d24655ffe52723ed19b6db15b7d61112425, generated from 1.0.0, edited false\n" +
	"instructions.reviewer: not configured, file none, 819 bytes, sha256 3c68950054e6429d33bc8a5a3e9539ba3ceb6f9131013065d1df6e0ea530b6ce, generated from 1.0.0, edited false\n" +
	"skills: none imported\n"

const validSettingKeys = "approval_max_tasks, approval_budget, approval_classes, approval_protected_paths, gate_plan, gate_land, gate_push, plan_review, plan_review.tier, plan_review.effort, approval_tiers, approval_restarted, land_target, after_land, land_ready, default_check, worker_tools, setup, setup_cache_key_files, setup_timeout_minutes, extra_repos, foreman_rotate_tokens, foreman_wakes_per_day, plan_files, instructions"

// TestProjectSettingsGolden covers project set and show: the defaults,
// one override, the reset, and the usage errors for a bad key and value.
func TestProjectSettingsGolden(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	repo := "https://example.invalid/settings.git"

	do := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		// Flags precede the key=value pairs: the first positional ends them.
		full := append([]string{args[0], args[1], "--state-dir", state}, args[2:]...)
		code := run(ctx, full, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	// No project yet: show says so.
	if code, _, stderr := do("project", "show", "--repo", repo); code != 1 || !strings.Contains(stderr, "no project for repo "+repo) {
		t.Fatalf("show without a project: code %d, stderr %q", code, stderr)
	}

	// An override creates the project; the others stay defaults.
	code, stdout, stderr := do("project", "set", "--repo", repo, "approval_protected_paths=infra/**+db/*.sql")
	if code != 0 {
		t.Fatalf("set exit = %d, stderr = %q", code, stderr)
	}
	want := "approval_max_tasks=0 (default)\n" +
		"approval_budget=none (default)\n" +
		"approval_classes=implement+review+answer (default)\n" +
		"approval_protected_paths=infra/**+db/*.sql (set)\n" +
		gateDefaults +
		"default_check=none (default)\n" +
		"worker_tools=agent default (default)\n" + tailDefaults
	if stdout != want {
		t.Fatalf("set printed %q, want %q", stdout, want)
	}

	// Show reads the same back.
	if code, stdout, stderr = do("project", "show", "--repo", repo); code != 0 || stdout != want+guidanceDefaults {
		t.Fatalf("show = %d %q (stderr %q), want %q", code, stdout, stderr, want)
	}

	// Two keys at once; an unnamed key keeps its value.
	code, stdout, stderr = do("project", "set", "--repo", repo, "approval_max_tasks=5", "approval_budget=2000000+$5.50")
	want = "approval_max_tasks=5 (set)\n" +
		"approval_budget=2000000+$5.5 (set)\n" +
		"approval_classes=implement+review+answer (default)\n" +
		"approval_protected_paths=infra/**+db/*.sql (set)\n" +
		gateDefaults +
		"default_check=none (default)\n" +
		"worker_tools=agent default (default)\n" + tailDefaults
	if code != 0 || stdout != want {
		t.Fatalf("second set = %d %q (stderr %q), want %q", code, stdout, stderr, want)
	}

	// An empty value resets to the default.
	code, stdout, stderr = do("project", "set", "--repo", repo, "approval_max_tasks=", "approval_budget=", "approval_classes=", "approval_protected_paths=")
	want = "approval_max_tasks=0 (default)\n" +
		"approval_budget=none (default)\n" +
		"approval_classes=implement+review+answer (default)\n" +
		"approval_protected_paths=none (default)\n" +
		gateDefaults +
		"default_check=none (default)\n" +
		"worker_tools=agent default (default)\n" + tailDefaults
	if code != 0 || stdout != want {
		t.Fatalf("reset = %d %q (stderr %q), want %q", code, stdout, stderr, want)
	}

	// A bad key is a usage error naming the valid keys and the help command.
	code, _, stderr = do("project", "set", "--repo", repo, "approval_max=3")
	wantErr := `brigada: invalid project setting: unknown key "approval_max"; valid keys: ` + validSettingKeys + "\nRun 'brigada project set --help' for usage.\n"
	if code != 2 || stderr != wantErr {
		t.Fatalf("bad key = %d %q, want 2 %q", code, stderr, wantErr)
	}

	// A bad value is a usage error naming what the key takes and the help command.
	code, _, stderr = do("project", "set", "--repo", repo, "approval_max_tasks=many")
	wantErr = `brigada: invalid project setting: approval_max_tasks takes an integer (0 = off, the default; negative = every approval rule off), got "many"; valid keys: ` + validSettingKeys + "\nRun 'brigada project set --help' for usage.\n"
	if code != 2 || stderr != wantErr {
		t.Fatalf("bad value = %d %q, want 2 %q", code, stderr, wantErr)
	}

	// Nothing was stored by the refused calls.
	if code, stdout, _ = do("project", "show", "--repo", repo); code != 0 || strings.Contains(stdout, "(set)") {
		t.Fatalf("show after refusals = %d %q, want all defaults", code, stdout)
	}

	// A pair without "=" and a set without pairs are usage errors too.
	if code, _, stderr = do("project", "set", "--repo", repo, "approval_max_tasks"); code != 2 || !strings.Contains(stderr, "key=value") {
		t.Fatalf("no equals = %d %q", code, stderr)
	}
	if code, _, stderr = do("project", "set", "--repo", repo); code != 2 || !strings.Contains(stderr, "valid keys: approval_max_tasks") {
		t.Fatalf("no pairs = %d %q", code, stderr)
	}
}

// TestProjectSettingsGates covers the gate and land keys (§6.3): a set
// and show round-trip, the reset, and the refusals for bad values.
func TestProjectSettingsGates(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	repo := "https://example.invalid/gates.git"
	do := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		full := append([]string{args[0], args[1], "--state-dir", state}, args[2:]...)
		code := run(ctx, full, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	code, stdout, stderr := do("project", "set", "--repo", repo,
		"gate_plan=ask", "gate_land=ask", "gate_push=never",
		"approval_tiers=light+heavy", "approval_restarted=true",
		"land_target=brigada/landed", "after_land=make announce", "land_ready=.brigada/land-ready.sh")
	if code != 0 {
		t.Fatalf("set exit = %d, stderr = %q", code, stderr)
	}
	for _, line := range []string{
		"gate_plan=ask (set)\n", "gate_land=ask (set)\n", "gate_push=never (set)\n",
		"approval_tiers=light+heavy (set)\n", "approval_restarted=true (set)\n",
		"land_target=brigada/landed (set)\n", "after_land=make announce (set)\n",
		"land_ready=.brigada/land-ready.sh (set)\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("set printed %q, want the line %q", stdout, line)
		}
	}
	if code, stdout, _ = do("project", "show", "--repo", repo); code != 0 || !strings.Contains(stdout, "gate_push=never (set)") {
		t.Fatalf("show = %d %q", code, stdout)
	}

	// An empty value resets each key to its default.
	if code, stdout, stderr = do("project", "set", "--repo", repo,
		"gate_plan=", "gate_land=", "gate_push=", "approval_tiers=",
		"approval_restarted=", "land_target=", "after_land=", "land_ready="); code != 0 {
		t.Fatalf("reset = %d %q", code, stderr)
	}
	for _, line := range strings.Split(gateDefaults, "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(stdout, line) {
			t.Errorf("reset printed %q, want the default line %q", stdout, line)
		}
	}

	// Bad values are usage errors naming what the key takes.
	for _, pair := range []string{
		"gate_plan=maybe", "gate_land=never", "gate_push=always",
		"approval_tiers=huge", "approval_restarted=maybe", "land_target=bad..name",
	} {
		if code, _, stderr = do("project", "set", "--repo", repo, pair); code != 2 || !strings.Contains(stderr, "invalid project setting") {
			t.Fatalf("set %s = %d %q, want a usage refusal", pair, code, stderr)
		}
	}
}

// TestProjectSettingsDefaultRepo takes the repo from the working
// directory's origin.
func TestProjectSettingsDefaultRepo(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	repo := "https://example.invalid/origin.git"
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", repo}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v: %s", args, err, out)
		}
	}
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"project", "set", "--state-dir", state, "approval_max_tasks=-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("set exit = %d, stderr = %q", code, stderr.String())
	}
	stdout.Reset()
	if code := run(ctx, []string{"project", "show", "--repo", repo, "--state-dir", state}, &stdout, &stderr); code != 0 ||
		!strings.HasPrefix(stdout.String(), "approval_max_tasks=-1 (set)\n") {
		t.Fatalf("show = %d %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
