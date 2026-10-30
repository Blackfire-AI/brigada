package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

type ciStep struct{ name, run string }

// buildSteps reads the workflow's build job, including steps after test so
// duplicate test steps and unsupported multiline runs cannot go unnoticed.
// A pre-test step whose shape it cannot read is an error, never a dropped key.
func buildSteps(workflow string) ([]ciStep, error) {
	jobRE := regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*(?:#.*)?$`)
	var steps []ciStep
	var keys [][]string // the keys each step declares, parallel to steps
	inJobs, inBuild, inSteps := false, false, false
	builds := 0
	cur := "" // the step key the current line belongs to
	for _, line := range strings.Split(workflow, "\n") {
		if line == "jobs:" {
			inJobs = true
			continue
		}
		if inJobs && len(line) > 0 && line[0] != ' ' && line[0] != '#' {
			inJobs, inBuild, inSteps = false, false, false
		}
		if !inJobs {
			continue
		}
		if m := jobRE.FindStringSubmatch(line); m != nil {
			inBuild, inSteps = m[1] == "build", false
			if inBuild {
				builds++
			}
			continue
		}
		if !inBuild {
			continue
		}
		if line == "    steps:" {
			inSteps = true
			continue
		}
		if inSteps && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "      ") && strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			inSteps = false
		}
		if !inSteps {
			continue
		}
		if strings.HasPrefix(line, "      - ") {
			steps = append(steps, ciStep{})
			keys = append(keys, nil)
			cur = ""
			line = "        " + strings.TrimPrefix(line, "      - ")
		}
		if len(steps) == 0 || strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		last := len(steps) - 1
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent < 8 {
			return nil, fmt.Errorf("unsupported line in build job: %q; update the test", line)
		}
		if indent > 8 {
			// Nested lines belong to the key above them; a with: block is the
			// only one a pre-test step may carry (its values are not run).
			if cur != "with" {
				keys[last] = append(keys[last], "nested "+cur)
			}
			continue
		}
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		cur = strings.TrimSpace(key)
		keys[last] = append(keys[last], cur)
		value = strings.TrimSpace(value)
		switch cur {
		case "name":
			steps[last].name = value
		case "run":
			if value == "" {
				return nil, fmt.Errorf("multiline run in build step; update the test")
			}
			steps[last].run = value
		}
	}
	if builds == 0 {
		return nil, fmt.Errorf("missing build job")
	}
	if builds != 1 {
		return nil, fmt.Errorf("duplicate build job")
	}
	for _, step := range steps {
		if step.run == "" || strings.HasPrefix(step.run, "|") || strings.HasPrefix(step.run, ">") {
			if step.run != "" {
				return nil, fmt.Errorf("multiline run in build step %q; update the test", step.name)
			}
		}
	}
	tests, testIndex := 0, -1
	for i, step := range steps {
		if step.name == "test" {
			tests++
			testIndex = i
		}
	}
	if tests == 0 {
		return nil, fmt.Errorf("missing test step in build job")
	}
	if tests != 1 {
		return nil, fmt.Errorf("duplicate test step in build job")
	}
	for i := range steps[:testIndex] {
		if err := checkPreTestShape(steps[i], keys[i]); err != nil {
			return nil, err
		}
	}
	pre := 0
	for _, step := range steps[:testIndex] {
		if step.run != "" {
			pre++
		}
	}
	if pre == 0 {
		return nil, fmt.Errorf("empty pre-test steps in build job")
	}
	return steps, nil
}

// checkPreTestShape accepts a pre-test step only as a name plus single-line
// run, or as a uses step with an optional with block. Any other key would be
// silently dropped from the run list, so it is an error naming the step.
func checkPreTestShape(step ciStep, keys []string) error {
	label := step.name
	if label == "" {
		label = step.run
	}
	hasRun := slices.Contains(keys, "run")
	allowed := []string{"name", "run"}
	if !hasRun {
		if !slices.Contains(keys, "uses") {
			return fmt.Errorf("unsupported pre-test step %q in build job: no run or uses; update the test", label)
		}
		allowed = []string{"name", "uses", "with"}
	}
	for _, key := range keys {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unsupported pre-test step %q in build job: key %q; update the test", label, key)
		}
	}
	return nil
}

func ciCheckDrift(steps []ciStep, check string) error {
	var want []string
	for _, step := range steps {
		if step.name == "test" {
			if step.run != "go test ./..." {
				return fmt.Errorf("CI test step changed: %q", step.run)
			}
			want = append(want, step.run)
			if !reflect.DeepEqual(strings.Split(check, " && "), want) {
				return fmt.Errorf("default_check does not match CI: got %q, want %q", check, strings.Join(want, " && "))
			}
			return nil
		}
		if step.run != "" && step.name != "shellcheck" {
			want = append(want, step.run)
		}
	}
	return fmt.Errorf("missing test step")
}

func TestCICheckDrift(t *testing.T) {
	base := []ciStep{{"gofmt", `test -z "$(gofmt -l .)"`}, {"shellcheck", "shellcheck install.sh"}, {"guard", "go run ./internal/tools/guard"}, {"vet", "go vet ./..."}, {"test", "go test ./..."}}
	check := `test -z "$(gofmt -l .)" && go run ./internal/tools/guard && go vet ./... && go test ./...`
	modified := func(f func([]ciStep) []ciStep) []ciStep { return f(append([]ciStep(nil), base...)) }
	cases := []struct {
		name  string
		steps []ciStep
		check string
		ok    bool
	}{
		{"exact match", base, check, true},
		{"guard missing", base, strings.Replace(check, "go run ./internal/tools/guard && ", "", 1), false},
		{"vet before guard", base, strings.Replace(check, "go run ./internal/tools/guard && go vet ./...", "go vet ./... && go run ./internal/tools/guard", 1), false},
		{"vet ignored", base, strings.Replace(check, "go vet ./...", "go vet ./... || true", 1), false},
		{"semicolon", base, strings.Replace(check, " && ", "; ", 1), false},
		{"gofmt piped", base, strings.Replace(check, `test -z "$(gofmt -l .)"`, `test -z "$(gofmt -l .)" | tee output`, 1), false},
		{"trailing ampersand", base, check + " &", false},
		{"trailing newline", base, check + "\n", false},
		{"extra command", base, check + " && true", false},
		{"test not last", base, strings.Replace(check, "go vet ./... && go test ./...", "go test ./... && go vet ./...", 1), false},
		{"new CI step", modified(func(s []ciStep) []ciStep {
			return append(append(s[:4:4], ciStep{"staticcheck", "staticcheck ./..."}), s[4:]...)
		}), check, false},
		{"changed CI gofmt", modified(func(s []ciStep) []ciStep { s[0].run = "gofmt -w ."; return s }), check, false},
		{"shellcheck removed", modified(func(s []ciStep) []ciStep { return append(s[:1:1], s[2:]...) }), check, true},
		{"unnamed staticcheck run missing from check", modified(func(s []ciStep) []ciStep {
			return append(append(s[:4:4], ciStep{"", "staticcheck ./..."}), s[4:]...)
		}), check, false},
		{"unnamed staticcheck run in order", modified(func(s []ciStep) []ciStep {
			return append(append(s[:4:4], ciStep{"", "staticcheck ./..."}), s[4:]...)
		}), strings.Replace(check, "go vet ./... && ", "go vet ./... && staticcheck ./... && ", 1), true},
		{"unnamed staticcheck run out of order", modified(func(s []ciStep) []ciStep {
			return append(append(s[:4:4], ciStep{"", "staticcheck ./..."}), s[4:]...)
		}), strings.Replace(check, "go vet ./... && ", "staticcheck ./... && go vet ./... && ", 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ciCheckDrift(tc.steps, tc.check)
			if (err == nil) != tc.ok {
				t.Fatalf("ciCheckDrift: %v", err)
			}
		})
	}
}

func TestBuildStepsErrors(t *testing.T) {
	const prefix = "jobs:\n  build:\n    steps:\n"
	const testStep = "      - name: test\n        run: go test ./...\n"
	cases := []struct{ name, workflow, want string }{
		{"no build", "jobs:\n", "missing build job"},
		{"renamed build", "jobs:\n  compile:\n    steps:\n", "missing build job"},
		{"renamed test", prefix + "      - name: check\n        run: go test ./...\n", "missing test step"},
		{"duplicate test", prefix + "      - name: test\n        run: go test ./...\n      - name: test\n        run: go test ./...\n", "duplicate test step"},
		{"no pre-test", prefix + "      - name: test\n        run: go test ./...\n", "empty pre-test"},
		{"duplicate build", prefix + "  build:\n    steps:\n", "duplicate build job"},
		{"multiline run", prefix + "      - name: format\n        run: |\n          gofmt .\n", "update the test"},
		{"pre-test env key", prefix + "      - name: format\n        env:\n          X: \"1\"\n        run: gofmt .\n" + testStep, `unsupported pre-test step "format" in build job: key "env"`},
		{"pre-test shell key", prefix + "      - name: format\n        run: gofmt .\n        shell: bash\n" + testStep, `unsupported pre-test step "format" in build job: key "shell"`},
		{"unnamed pre-test if key", prefix + "      - run: gofmt .\n        if: always()\n" + testStep, `unsupported pre-test step "gofmt ." in build job: key "if"`},
		{"pre-test run continuation", prefix + "      - name: format\n        run: gofmt \\\n          -l .\n" + testStep, `unsupported pre-test step "format" in build job: key "nested run"`},
		{"pre-test name only", prefix + "      - name: format\n" + testStep, `unsupported pre-test step "format" in build job: no run or uses`},
		{"pre-test uses with run", prefix + "      - name: format\n        uses: actions/checkout@v4\n        run: gofmt .\n" + testStep, `unsupported pre-test step "format" in build job: key "uses"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSteps(tc.workflow)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestCICheckMatchesProject(t *testing.T) {
	workflow, err := os.ReadFile(docPath(".github/workflows/ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	steps, err := buildSteps(string(workflow))
	if err != nil {
		t.Fatal(err)
	}
	project, err := os.ReadFile(docPath(engine.ProjectConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := engine.ParseProjectConfig(project)
	if err != nil {
		t.Fatal(err)
	}
	var check string
	if err := json.Unmarshal(cfg.Values[engine.SettingDefaultCheck], &check); err != nil {
		t.Fatal(err)
	}
	if err := ciCheckDrift(steps, check); err != nil {
		t.Fatal(err)
	}
}
