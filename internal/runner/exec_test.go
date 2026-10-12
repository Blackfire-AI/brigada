package runner

import (
	"os"
	"slices"
	"testing"
)

func TestScrubEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "GIT_ASKPASS=/secret/askpass", "HOME=/home/worker", "TOKEN_VAR=x"}

	// Named keys are gone; everything else survives in order.
	got := scrubEnv(slices.Clone(base), []string{"GIT_ASKPASS=/secret/askpass", "TOKEN_VAR=y"})
	if want := []string{"PATH=/usr/bin", "HOME=/home/worker"}; !slices.Equal(got, want) {
		t.Errorf("scrubEnv = %v, want %v", got, want)
	}

	// Nothing to drop leaves the slice untouched.
	if got := scrubEnv(base, nil); !slices.Equal(got, base) {
		t.Errorf("scrubEnv with no drops = %v, want %v", got, base)
	}

	// A nil env means inherit the process's; the scrub still applies,
	// so an inherited credential cannot slip through either.
	os.Setenv("BRIGADA_TEST_SCRUB_ME", "hush")
	t.Cleanup(func() { os.Unsetenv("BRIGADA_TEST_SCRUB_ME") })
	got = scrubEnv(nil, []string{"BRIGADA_TEST_SCRUB_ME=hush"})
	if slices.Contains(got, "BRIGADA_TEST_SCRUB_ME=hush") {
		t.Error("an inherited credential variable survived the scrub")
	}
	if !slices.Contains(got, "PATH=") && !slices.ContainsFunc(got, func(kv string) bool {
		return len(kv) > 5 && kv[:5] == "PATH="
	}) {
		t.Error("the scrub wiped the whole environment")
	}

	// Entries without '=' are not variable names and are ignored.
	if got := scrubEnv(base, []string{"NOT_AN_ASSIGNMENT"}); !slices.Equal(got, base) {
		t.Errorf("a malformed drop entry changed the environment: %v", got)
	}
}
