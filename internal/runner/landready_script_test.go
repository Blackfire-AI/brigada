package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH stands in for gh: it records its arguments, applies the --jq
// filter with the real jq to the JSON it is given, as gh does, and
// prints the result. The test asserts the recorded arguments afterwards.
const fakeGH = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_GH_ARGS"
case "$1 $2" in
  'api repos/{owner}/{repo}/branches/brigada%2Flanded') json='{"commit":{"sha":"current","commit":{"committer":{"date":"2099-01-01T00:00:00Z"}}}}' ;;
  'pr list') json='[]' ;;
  'run list') [ "$FAKE_GH_EXIT" = 0 ] || exit "$FAKE_GH_EXIT"; json=$FAKE_GH_JSON ;;
  *) exit 1 ;;
esac
filter=
while [ $# -gt 0 ]; do
  case $1 in
  --jq) filter=$2; shift ;;
  esac
  shift
done
printf '%s' "$json" | jq -r "$filter"
`

// TestLandReadyScript runs scripts/land-ready.sh against a fake gh and
// checks the invocation and the exit-code table the land_ready gate reads
// (issue #87).
func TestLandReadyScript(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	script, err := filepath.Abs("../../scripts/land-ready.sh")
	if err != nil {
		t.Fatal(err)
	}
	run := func(status, conclusion, url string) string {
		return `[{"headSha":"current","status":"` + status + `","conclusion":"` + conclusion + `","url":"` + url + `"}]`
	}
	cases := []struct {
		name     string
		json     string
		ghExit   string
		wantExit int
		wantLine string
	}{
		{"no runs", `[]`, "0", 2, ""},
		{"success", run("completed", "success", "https://x/runs/1"), "0", 0, ""},
		{"failure", run("completed", "failure", "https://x/runs/2"), "0", 1, "https://x/runs/2"},
		{"cancelled", run("completed", "cancelled", "https://x/runs/3"), "0", 1, "https://x/runs/3"},
		{"skipped", run("completed", "skipped", "https://x/runs/6"), "0", 1, "https://x/runs/6"},
		{"in progress", run("in_progress", "", "https://x/runs/4"), "0", 2, ""},
		{"queued", run("queued", "", "https://x/runs/5"), "0", 2, ""},
		{"gh fails", `[]`, "1", 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeGH), 0o755); err != nil {
				t.Fatal(err)
			}
			argsFile := filepath.Join(dir, "args")
			cmd := exec.Command("sh", script)
			cmd.Env = append(os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"FAKE_GH_ARGS="+argsFile, "FAKE_GH_JSON="+c.json, "FAKE_GH_EXIT="+c.ghExit)
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
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"run list", "--branch brigada/landed", "--limit 100", "--json status,conclusion,url,headSha", "--jq "} {
				if !strings.Contains(string(args), want) {
					t.Fatalf("gh args %q lack %q", args, want)
				}
			}
		})
	}
}
