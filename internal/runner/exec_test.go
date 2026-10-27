package runner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestScrubWorkerEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/worker", "TOKEN_VAR=x", "BRIGADA_KEEP=1"}

	// Variables GitEnv names are gone; everything else survives in order.
	got := scrubWorkerEnv(slices.Clone(base), []string{"TOKEN_VAR=y"})
	if want := []string{"PATH=/usr/bin", "HOME=/home/worker", "BRIGADA_KEEP=1"}; !slices.Equal(got, want) {
		t.Errorf("scrubWorkerEnv = %v, want %v", got, want)
	}

	// Nothing named leaves a clean environment untouched.
	if got := scrubWorkerEnv(base, nil); !slices.Equal(got, base) {
		t.Errorf("scrubWorkerEnv with no drops = %v, want %v", got, base)
	}

	// A nil env means inherit the process's; the scrub still applies,
	// so an inherited credential cannot slip through either.
	t.Setenv("BRIGADA_TEST_SCRUB_ME", "hush")
	got = scrubWorkerEnv(nil, []string{"BRIGADA_TEST_SCRUB_ME=hush"})
	if slices.Contains(got, "BRIGADA_TEST_SCRUB_ME=hush") {
		t.Error("an inherited credential variable survived the scrub")
	}
	if !slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		t.Error("the scrub wiped the whole environment")
	}

	// Entries without '=' are not variable names and are ignored.
	if got := scrubWorkerEnv(base, []string{"NOT_AN_ASSIGNMENT"}); !slices.Equal(got, base) {
		t.Errorf("a malformed drop entry changed the environment: %v", got)
	}
}

// fakeCredentialEnv sets one value for each family of ambient credential
// and returns their names. The values are built at run time and are not
// real.
func fakeCredentialEnv(t *testing.T) []string {
	t.Helper()
	fake := "fake-" + strings.Repeat("x", 6)
	names := []string{
		"GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK", "GIT_ASKPASS", "NPM_TOKEN",
		"AWS_" + "SECRET_ACCESS_KEY", "AWS_" + "SESSION_TOKEN", "AZURE_CLIENT_SECRET",
		"GOOGLE_APPLICATION_CREDENTIALS", "GIT_CONFIG_VALUE_0",
	}
	for _, n := range names {
		t.Setenv(n, fake)
	}
	return names
}

// TestScrubWorkerEnvDropsAmbientCredentials: with no GitEnv at all — the
// production configuration — the user's forge, cloud and registry
// credentials still stay out of a worker's environment (§5.8), while
// what the CLI needs to run is kept.
func TestScrubWorkerEnvDropsAmbientCredentials(t *testing.T) {
	names := fakeCredentialEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "fake-model-login")
	t.Setenv("GOCACHE", "/tmp/gocache")

	got := scrubWorkerEnv(nil, nil)
	for _, n := range names {
		if slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, n+"=") }) {
			t.Errorf("%s reached the worker's environment", n)
		}
	}
	for _, keep := range []string{"ANTHROPIC_API_KEY=fake-model-login", "GOCACHE=/tmp/gocache"} {
		if !slices.Contains(got, keep) {
			t.Errorf("%s was scrubbed from the worker's environment", keep)
		}
	}
	if !slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		t.Error("PATH is missing from the worker's environment")
	}
}

// TestScrubWorkerEnvDropsSelfHostedForgeTokens: the ambient tokens of the
// self-hosted forges leave a worker's environment and a command's, like the
// rest of the forge credentials (§5.8). The values are not real.
func TestScrubWorkerEnvDropsSelfHostedForgeTokens(t *testing.T) {
	names := []string{
		"GITEA_TOKEN", "GITEA_ACCESS_TOKEN",
		"FORGEJO_TOKEN", "TEA_TOKEN",
	}
	fake := "fake-" + strings.Repeat("x", 6)
	for _, n := range names {
		t.Setenv(n, fake)
	}

	envs := []struct {
		kind string
		kvs  []string
	}{
		{"worker", scrubWorkerEnv(nil, nil)},
		{"command", scrubCommandEnv(nil, nil)},
	}
	for _, n := range names {
		for _, env := range envs {
			if slices.ContainsFunc(env.kvs, func(kv string) bool { return strings.HasPrefix(kv, n+"=") }) {
				t.Errorf("%s reached the %s's environment", n, env.kind)
			}
		}
	}
	for _, env := range envs {
		if !slices.ContainsFunc(env.kvs, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
			t.Errorf("the scrub wiped the %s's whole environment", env.kind)
		}
	}
}

// TestCommandEnvScrubsChecksAndSetup: a check or a setup command runs
// with the worker's scrub plus no brigada variables, and still has PATH,
// HOME, the Go caches and the job's own extra variables (§5.8).
func TestCommandEnvScrubsChecksAndSetup(t *testing.T) {
	names := fakeCredentialEnv(t)
	t.Setenv("BRIGADA_TOKEN", "fake-runner-secret")
	t.Setenv("BRIGADA_STATE_THING", "1")
	t.Setenv("GOCACHE", "/tmp/gocache")
	cfg := &Config{GitEnv: []string{"GIT_RUNNER_ONLY=1"}}
	t.Setenv("GIT_RUNNER_ONLY", "1")

	env := cfg.commandEnv("BRIGADA_REPO_SCHEMA=/work/schema", "BRIGADA_CACHE_DIR=/work/cache")
	has := func(prefix string) bool {
		return slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, prefix) })
	}
	for _, n := range append(names, "BRIGADA_TOKEN", "BRIGADA_STATE_THING", "GIT_RUNNER_ONLY") {
		if has(n + "=") {
			t.Errorf("%s reached the command's environment", n)
		}
	}
	for _, keep := range []string{"PATH=", "HOME=", "GOCACHE=", "BRIGADA_REPO_SCHEMA=/work/schema", "BRIGADA_CACHE_DIR=/work/cache"} {
		if !has(keep) {
			t.Errorf("%s is missing from the command's environment", keep)
		}
	}

	// And through a real check.
	dir := t.TempDir()
	res := runCheckIn(context.Background(), dir,
		`test -z "$GH_TOKEN" && test -z "$BRIGADA_TOKEN" && test -n "$PATH" && test "$BRIGADA_REPO_SCHEMA" = /work/schema`,
		5*time.Second, env)
	if res.Failed() {
		t.Errorf("the check saw a credential or lost a variable: exit %d, %q", res.ExitCode, res.Output)
	}
}
