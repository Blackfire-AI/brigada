package version

import (
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	if Version == "" {
		t.Fatal("Version is empty")
	}
	if strings.ContainsAny(Version, " \t\n") {
		t.Fatalf("Version %q contains whitespace", Version)
	}
}
