package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestBudgetCommand covers `brigada budget` (§4.3): the show, the set
// through the settings path, the single-project --repo default, and the
// usage errors for an out-of-bounds --wakes and positional arguments.
func TestBudgetCommand(t *testing.T) {
	ctx, state := startSettingsEngine(t)
	repo := "https://example.invalid/budget.git"
	shown := "example.invalid/budget" // the canonical identity the engine keeps

	do := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		full := append([]string{"budget", "--state-dir", state}, args...)
		code := run(ctx, full, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	// Show with no project: the engine says so.
	if code, _, stderr := do(); code != 1 || !strings.Contains(stderr, "no project yet") {
		t.Fatalf("show with no project: code %d, stderr %q", code, stderr)
	}

	// The set creates the project and prints the budget as it stands.
	code, stdout, stderr := do("--repo", repo, "--wakes", "5")
	if code != 0 {
		t.Fatalf("set exit = %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"foreman wake budget set to 5 per day\n",
		shown + ": foreman wake budget 5 per day (set), 0 used today, resets ",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("set printed %q, want it to carry %q", stdout, want)
		}
	}

	// The show reads it back; with one project, no --repo names it.
	if code, stdout, stderr = do("--repo", repo); code != 0 ||
		!strings.Contains(stdout, shown+": foreman wake budget 5 per day (set), 0 used today") {
		t.Fatalf("show after the set = %d %q (stderr %q)", code, stdout, stderr)
	}
	if code, stdout, stderr = do(); code != 0 ||
		!strings.Contains(stdout, shown+": foreman wake budget 5 per day (set)") {
		t.Fatalf("show without --repo = %d %q (stderr %q)", code, stdout, stderr)
	}

	// The bounds are usage errors, named before the engine is contacted.
	for _, bad := range []string{"0", "-1", strconv.Itoa(engine.MaxForemanWakesPerDay + 1)} {
		if code, _, stderr = do("--repo", repo, "--wakes", bad); code != 2 ||
			!strings.Contains(stderr, "positive whole number of foreman wakes per day") {
			t.Fatalf("--wakes %s = %d %q, want 2 with the bound named", bad, code, stderr)
		}
	}

	// Positional arguments — the design's `budget <plan> +<minutes>` —
	// are not this command's; the wake budget is a flag.
	if code, _, stderr = do("7", "+30"); code != 2 || !strings.Contains(stderr, "no positional arguments") {
		t.Fatalf("positional arguments = %d %q, want 2", code, stderr)
	}

	// Nothing was stored by the refused calls.
	if code, stdout, _ = do("--repo", repo); code != 0 || !strings.Contains(stdout, "5 per day (set)") {
		t.Fatalf("show after the refusals = %d %q, want the budget of 5 kept", code, stdout)
	}
}
