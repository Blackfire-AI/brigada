package claude

import (
	"reflect"
	"testing"
)

func TestShellCommands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		tools []string
		want  []string
		whole bool
	}{
		{"default", nil, []string{"go build", "go test", "go vet", "gofmt"}, false},
		{"none", []string{}, nil, false},
		{"explicit, Edit left out", []string{"Edit", "Bash(npm test:*)", "Bash(git diff:*)"}, []string{"npm test", "git diff"}, false},
		{"exact rule gives its command", []string{"Bash(git status)", "Bash()", "Read"}, []string{"git status"}, false},
		{"whole shell", []string{"Edit", "Bash"}, nil, true},
	} {
		got, whole := (&Adapter{AllowedTools: tc.tools}).ShellCommands([]string{"Bash(ignored:*)"})
		if !reflect.DeepEqual(got, tc.want) || whole != tc.whole {
			t.Errorf("%s: got %v, %v; want %v, %v", tc.name, got, whole, tc.want, tc.whole)
		}
	}
}
