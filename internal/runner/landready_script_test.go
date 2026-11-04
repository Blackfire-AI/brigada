package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
  'api repos/{owner}/{repo}/actions/runs/'*'/jobs?per_page=100') [ "${FAKE_GH_JOBS_EXIT:-0}" = 0 ] || exit "$FAKE_GH_JOBS_EXIT"; json=$FAKE_GH_JOBS ;;
  'run rerun') [ -z "$FAKE_GH_RERUN_ERR" ] || { printf '%s\n' "$FAKE_GH_RERUN_ERR" >&2; exit 1; }; exit 0 ;;
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
		return `[{"headSha":"current","status":"` + status + `","conclusion":"` + conclusion + `","url":"` + url + `","databaseId":77,"attempt":1}]`
	}
	runAt := func(attempt int, url string) string {
		return `[{"headSha":"current","status":"completed","conclusion":"failure","url":"` + url + `","databaseId":77,"attempt":` + strconv.Itoa(attempt) + `}]`
	}
	// job renders one job; steps are the step conclusions ("" is null).
	job := func(name, status, conclusion string, steps ...string) string {
		var ss []string
		for _, c := range steps {
			if c == "" {
				ss = append(ss, `{"conclusion":null}`)
			} else {
				ss = append(ss, `{"conclusion":"`+c+`"}`)
			}
		}
		return `{"name":"` + name + `","status":"` + status + `","conclusion":"` + conclusion + `","steps":[` + strings.Join(ss, ",") + `]}`
	}
	jobs := func(js ...string) string { return `{"jobs":[` + strings.Join(js, ",") + `]}` }
	pass := job("build", "completed", "success", "success")
	noRunner := job("test-macos", "completed", "cancelled")
	const rerun = "run rerun 77 --failed"
	const jobsRead = "actions/runs/77/jobs?per_page=100"
	const failURL = "https://x/runs/9"
	cases := []struct {
		name      string
		json      string
		jobs      string
		jobsExit  string
		rerunErr  string
		ghExit    string
		wantExit  int
		wantLine  string
		wantArgs  []string
		wantNoArg []string
	}{
		{name: "no runs", json: `[]`, ghExit: "0", wantExit: 2},
		{name: "success", json: run("completed", "success", "https://x/runs/1"), ghExit: "0", wantNoArg: []string{"actions/runs"}},
		{name: "failure", json: run("completed", "failure", "https://x/runs/2"), jobs: jobs(job("build", "completed", "failure", "failure")), ghExit: "0", wantExit: 1, wantLine: "https://x/runs/2", wantNoArg: []string{rerun}},
		{name: "cancelled", json: run("completed", "cancelled", "https://x/runs/3"), ghExit: "0", wantExit: 1, wantLine: "https://x/runs/3"},
		{name: "skipped", json: run("completed", "skipped", "https://x/runs/6"), ghExit: "0", wantExit: 1, wantLine: "https://x/runs/6"},
		{name: "in progress", json: run("in_progress", "", "https://x/runs/4"), ghExit: "0", wantExit: 2, wantNoArg: []string{"run rerun", jobsRead}},
		{name: "queued", json: run("queued", "", "https://x/runs/5"), ghExit: "0", wantExit: 2, wantNoArg: []string{"run rerun", jobsRead}},
		{name: "gh fails", json: `[]`, ghExit: "1", wantExit: 2},
		{name: "rerun issued", json: runAt(1, failURL), jobs: jobs(pass, noRunner), ghExit: "0", wantExit: 2, wantArgs: []string{rerun, jobsRead}},
		{name: "rerun issued on the second attempt", json: runAt(2, failURL), jobs: jobs(pass, noRunner), ghExit: "0", wantExit: 2, wantArgs: []string{rerun}},
		{name: "attempts spent", json: runAt(3, failURL), jobs: jobs(pass, noRunner), ghExit: "0", wantExit: 1, wantLine: "test-macos got no runner after 3 attempts", wantNoArg: []string{"run rerun"}},
		{name: "two no-runner jobs", json: runAt(3, failURL), jobs: jobs(noRunner, job("test-windows", "completed", "cancelled", ""), pass), ghExit: "0", wantExit: 1, wantLine: "test-macos, test-windows got no runner after 3 attempts", wantNoArg: []string{"run rerun"}},
		{name: "failure job beside no-runner job", json: runAt(1, failURL), jobs: jobs(job("test", "completed", "failure", "failure"), noRunner), ghExit: "0", wantExit: 1, wantLine: failURL, wantNoArg: []string{"run rerun"}},
		{name: "cancelled job that ran a step", json: runAt(1, failURL), jobs: jobs(pass, job("test-macos", "completed", "cancelled", "success", "")), ghExit: "0", wantExit: 1, wantLine: failURL, wantNoArg: []string{"run rerun"}},
		{name: "cancelled job with a cancelled step", json: runAt(1, failURL), jobs: jobs(pass, job("test-macos", "completed", "cancelled", "cancelled")), ghExit: "0", wantExit: 1, wantLine: failURL, wantNoArg: []string{"run rerun"}},
		{name: "cancelled job with only skipped steps", json: runAt(1, failURL), jobs: jobs(pass, job("test-macos", "completed", "cancelled", "skipped", "skipped")), ghExit: "0", wantExit: 2, wantArgs: []string{rerun}},
		{name: "skipped job beside no-runner job", json: runAt(1, failURL), jobs: jobs(job("lint", "completed", "skipped"), noRunner), ghExit: "0", wantExit: 1, wantLine: failURL, wantNoArg: []string{"run rerun"}},
		{name: "empty jobs list", json: runAt(1, failURL), jobs: jobs(), ghExit: "0", wantExit: 1, wantLine: failURL, wantNoArg: []string{"run rerun"}},
		{name: "rerun refused", json: runAt(1, failURL), jobs: jobs(pass, noRunner), rerunErr: "HTTP 403: Resource not accessible", ghExit: "0", wantExit: 2, wantLine: "HTTP 403: Resource not accessible", wantArgs: []string{rerun}},
		{name: "jobs lookup fails", json: runAt(1, failURL), jobs: jobs(pass, noRunner), jobsExit: "1", ghExit: "0", wantExit: 2, wantArgs: []string{jobsRead}, wantNoArg: []string{"run rerun"}},
		{name: "job not completed", json: runAt(1, failURL), jobs: jobs(noRunner, job("test", "in_progress", "")), ghExit: "0", wantExit: 2, wantNoArg: []string{"run rerun"}},
		{name: "cancelled run with no-runner job", json: run("completed", "cancelled", failURL), jobs: jobs(pass, noRunner), ghExit: "0", wantExit: 1, wantLine: failURL, wantNoArg: []string{"run rerun", jobsRead}},
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
				"FAKE_GH_ARGS="+argsFile, "FAKE_GH_JSON="+c.json, "FAKE_GH_EXIT="+c.ghExit,
				"FAKE_GH_JOBS="+c.jobs, "FAKE_GH_JOBS_EXIT="+c.jobsExit, "FAKE_GH_RERUN_ERR="+c.rerunErr)
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
			for _, want := range c.wantArgs {
				if !strings.Contains(string(args), want) {
					t.Fatalf("gh args %q lack %q", args, want)
				}
			}
			for _, bad := range c.wantNoArg {
				if strings.Contains(string(args), bad) {
					t.Fatalf("gh args %q contain %q", args, bad)
				}
			}
		})
	}
}
