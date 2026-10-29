package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

type ciStep struct {
	name, run string
	uses      bool
}

// buildSteps reads the workflow's build job, including steps after test so
// duplicate test steps and unsupported multiline runs cannot go unnoticed.
func buildSteps(workflow string) ([]ciStep, error) {
	jobRE := regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*(?:#.*)?$`)
	var steps []ciStep
	inJobs, inBuild, inSteps := false, false, false
	builds := 0
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
			field := strings.TrimPrefix(line, "      - ")
			if strings.HasPrefix(field, "name: ") {
				steps[len(steps)-1].name = strings.TrimSpace(strings.TrimPrefix(field, "name: "))
			}
			if strings.HasPrefix(field, "run:") {
				steps[len(steps)-1].run = strings.TrimSpace(strings.TrimPrefix(field, "run:"))
				if steps[len(steps)-1].run == "" {
					return nil, fmt.Errorf("multiline run in build step %q; update the test", steps[len(steps)-1].name)
				}
			}
			if strings.HasPrefix(field, "uses: ") {
				steps[len(steps)-1].uses = true
			} else if !strings.HasPrefix(field, "name: ") && !strings.HasPrefix(field, "run:") {
				return nil, fmt.Errorf("unsupported build step %q; update the test", strings.TrimSpace(field))
			}
			continue
		}
		if len(steps) == 0 {
			continue
		}
		if strings.HasPrefix(line, "        name: ") {
			steps[len(steps)-1].name = strings.TrimSpace(strings.TrimPrefix(line, "        name: "))
		}
		if strings.HasPrefix(line, "        run:") {
			steps[len(steps)-1].run = strings.TrimSpace(strings.TrimPrefix(line, "        run:"))
			if steps[len(steps)-1].run == "" {
				return nil, fmt.Errorf("multiline run in build step; update the test")
			}
			if strings.HasPrefix(line, "        ") && !strings.HasPrefix(line, "        name:") && !strings.HasPrefix(line, "        run:") && !strings.HasPrefix(line, "        #") && strings.TrimSpace(line) != "" {
				return nil, fmt.Errorf("unsupported build step %q; update the test", steps[len(steps)-1].name)
			}
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
	pre := 0
	for _, step := range steps[:testIndex] {
		if step.uses && step.name == "" && step.run == "" {
			continue
		}
		if (step.run == "" && step.name != "shellcheck") || strings.Contains(step.run, "${{") {
			return nil, fmt.Errorf("unsupported build step %q before test; update the test", step.name)
		}
		if step.run != "" {
			pre++
		}
	}
	if pre == 0 {
		return nil, fmt.Errorf("empty pre-test steps in build job")
	}
	return steps, nil
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
	base := []ciStep{{name: "gofmt", run: `test -z "$(gofmt -l .)"`}, {name: "shellcheck", run: "shellcheck install.sh"}, {name: "guard", run: "go run ./internal/tools/guard"}, {name: "vet", run: "go vet ./..."}, {name: "test", run: "go test ./..."}}
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
			return append(append(s[:4:4], ciStep{name: "staticcheck", run: "staticcheck ./..."}), s[4:]...)
		}), check, false},
		{"changed CI gofmt", modified(func(s []ciStep) []ciStep { s[0].run = "gofmt -w ."; return s }), check, false},
		{"shellcheck removed", modified(func(s []ciStep) []ciStep { return append(s[:1:1], s[2:]...) }), check, true},
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
	cases := []struct{ name, workflow, want string }{
		{"no build", "jobs:\n", "missing build job"},
		{"renamed build", "jobs:\n  compile:\n    steps:\n", "missing build job"},
		{"renamed test", prefix + "      - name: check\n        run: go test ./...\n", "missing test step"},
		{"duplicate test", prefix + "      - name: test\n        run: go test ./...\n      - name: test\n        run: go test ./...\n", "duplicate test step"},
		{"no pre-test", prefix + "      - name: test\n        run: go test ./...\n", "empty pre-test"},
		{"duplicate build", prefix + "  build:\n    steps:\n", "duplicate build job"},
		{"multiline run", prefix + "      - name: format\n        run: |\n          gofmt .\n", "update the test"},
		{"unsupported pre-test step", prefix + "      - name: setup\n        with: value\n      - name: test\n        run: go test ./...\n", "setup"},
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

func TestBuildStepsUnnamedRun(t *testing.T) {
	const workflow = "jobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n      - run: staticcheck ./...\n      - name: test\n        run: go test ./...\n"
	steps, err := buildSteps(workflow)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		check string
		want  string
	}{
		{"check lacks unnamed run", "go test ./...", "does not match CI"},
		{"check includes unnamed run", "staticcheck ./... && go test ./...", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ciCheckDrift(steps, tc.check)
			if tc.want == "" && err != nil {
				t.Fatalf("ciCheckDrift: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestBuildStepsUnsupportedPreTestShape(t *testing.T) {
	const workflow = "jobs:\n  build:\n    steps:\n      - name: setup\n        run: ${{ matrix.command }}\n      - name: test\n        run: go test ./...\n"
	_, err := buildSteps(workflow)
	if err == nil || !strings.Contains(err.Error(), "setup") {
		t.Fatalf("got %v, want error naming unsupported step setup", err)
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
