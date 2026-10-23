package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter/fake"
	"github.com/Blackfire-AI/brigada/internal/api"
)

// A harness's resolved variables reach that harness's worker alone, and
// win over an ambient value of the same name.
func TestHarnessEnvScopedToHarness(t *testing.T) {
	t.Setenv("HARNESS_TEST_VAR", "ambient-value")
	for _, tc := range []struct {
		agent, want string
	}{
		{"dsh", "from-config"},
		{"fake", "ambient-value"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "worker.sh")
			body := "#!/bin/sh\ncat > /dev/null\nprintf %s \"$HARNESS_TEST_VAR\" > \"" + filepath.Join(dir, "seen") + "\"\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := &Config{HarnessEnv: map[string][]string{"dsh": {"HARNESS_TEST_VAR=from-config"}}}
			cfg.Clock.fill()
			r := &jobRun{cfg: cfg, grant: &api.JobGrant{Kind: "work", Agent: tc.agent, Prompt: "p"}}
			_ = r.runWorker(context.Background(), &fake.Adapter{Script: script}, "m", dir, nil)
			got, err := os.ReadFile(filepath.Join(dir, "seen"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != tc.want {
				t.Errorf("worker for %s saw %q, want %q", tc.agent, got, tc.want)
			}
		})
	}
}
