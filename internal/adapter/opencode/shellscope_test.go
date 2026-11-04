package opencode

import (
	"reflect"
	"testing"
)

func TestShellCommandsMatchBashRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		tools []string
		want  []string
	}{
		{"default", nil, []string{"go build", "go test", "go vet", "gofmt"}},
		{"mapped, rest left out", []string{"Edit", "Bash(make test:*)", "Bash(go *:*)", "Bash"}, []string{"make test"}},
		{"nothing maps", []string{"Edit"}, nil},
	} {
		got, whole := (&Adapter{}).ShellCommands(tc.tools)
		if !reflect.DeepEqual(got, tc.want) || whole {
			t.Errorf("%s: got %v, %v; want %v", tc.name, got, whole, tc.want)
		}
		rules := bashRules(tc.tools, false)
		if (rules == "deny") != (len(tc.want) == 0) {
			t.Errorf("%s: bashRules %v disagrees with the commands", tc.name, rules)
		}
	}
}
