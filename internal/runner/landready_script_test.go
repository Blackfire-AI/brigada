package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLandReadyScript runs scripts/land-ready.sh against a fake gh that
// answers with the line the script's --jq filter would print, and checks
// the exit-code table the land_ready gate reads (issue #87).
func TestLandReadyScript(t *testing.T) {
	script, err := filepath.Abs("../../scripts/land-ready.sh")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		out      string
		ghExit   string
		wantExit int
		wantLine string
	}{
		{"no runs", "none", "0", 0, ""},
		{"empty answer", "", "0", 0, ""},
		{"success", "completed|success|https://x/runs/1", "0", 0, ""},
		{"failure", "completed|failure|https://x/runs/2", "0", 1, "https://x/runs/2"},
		{"cancelled", "completed|cancelled|https://x/runs/3", "0", 1, "https://x/runs/3"},
		{"in progress", "in_progress||https://x/runs/4", "0", 2, ""},
		{"queued", "queued||https://x/runs/5", "0", 2, ""},
		{"gh fails", "", "1", 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			fake := "#!/bin/sh\nprintf '%s\\n' \"$FAKE_GH_OUT\"\nexit \"$FAKE_GH_EXIT\"\n"
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", script)
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_GH_OUT="+c.out, "FAKE_GH_EXIT="+c.ghExit)
			out, err := cmd.Output()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != c.wantExit {
				t.Fatalf("exit = %d, want %d (output %q)", code, c.wantExit, out)
			}
			if c.wantLine != "" {
				first, _, _ := strings.Cut(string(out), "\n")
				if first != c.wantLine {
					t.Fatalf("first line = %q, want %q", first, c.wantLine)
				}
			}
		})
	}
}
