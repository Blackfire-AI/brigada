package installscript

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLandReady(t *testing.T) {
	script, err := filepath.Abs("../../scripts/land-ready.sh")
	if err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
case "$1 $2" in
  'api repos/{owner}/{repo}/branches/brigada%2Flanded')
    if [ "$BRANCH_MISSING" = yes ]; then echo 'Not Found (HTTP 404)' >&2; exit 1; fi
    printf 'current|%s\n' "$COMMIT_DATE" ;;
  'api repos/{owner}/{repo}') echo repo ;;
  'pr list') printf '%s\n' "$PR" ;;
  'run list')
    case "$*" in *headSha*) ;; *) exit 3;; esac
    printf '%s\n' "$RUN" ;;
  *) exit 3 ;;
esac
`
	for _, tc := range []struct {
		name, pr, run string
		age           time.Duration
		missing       bool
		code          int
		first         string
		contains      string
	}{
		{"success", "none", "current|completed|success|https://example.com/run", 0, false, 0, "", ""},
		{"older within grace", "none", "old|completed|success|https://example.com/run", 0, false, 2, "", "current yet; newest run is for old"},
		{"older past grace", "42|MERGEABLE|main", "old|completed|success|https://example.com/run", time.Hour, false, 1, "no CI run for current after 900s: PR #42 MERGEABLE", ""},
		{"in progress", "none", "current|in_progress||https://example.com/run", 0, false, 2, "", "in_progress"},
		{"no run within grace", "none", "none", 0, false, 2, "", "no CI run for current yet"},
		{"no run past grace", "none", "none", time.Hour, false, 1, "no CI run for current after 900s: no open PR", ""},
		{"conflicting", "42|CONFLICTING|main", "none", 0, false, 1, "PR #42 conflicts with main: CI will not run", ""},
		{"unknown", "42|UNKNOWN|main", "none", time.Hour, false, 2, "", "UNKNOWN"},
		{"missing branch", "none", "none", 0, true, 0, "", ""},
		{"failed run", "none", "current|completed|failure|https://example.com/run", 0, false, 1, "https://example.com/run", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(stub), 0755); err != nil {
				t.Fatal(err)
			}
			missing := "no"
			if tc.missing {
				missing = "yes"
			}
			cmd := exec.Command("sh", script)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "BRANCH_MISSING="+missing, "COMMIT_DATE="+time.Now().UTC().Add(-tc.age).Format("2006-01-02T15:04:05Z"), "PR="+tc.pr, "RUN="+tc.run, "BRIGADA_LAND_READY_GRACE_SECONDS=900")
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code || (tc.first != "" && strings.Split(string(out), "\n")[0] != tc.first) || !strings.Contains(string(out), tc.contains) {
				t.Fatalf("got exit %d output %q; want exit %d first %q containing %q", code, out, tc.code, tc.first, tc.contains)
			}
		})
	}
}
