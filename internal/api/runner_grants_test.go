package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestJobGrantToolGrants: a task's tool grants ride the lease grant's
// wire shape, and a task without grants keeps the old shape — no
// tool_grants key at all (§12.3).
func TestJobGrantToolGrants(t *testing.T) {
	plain := jobGrantOf(&engine.LeaseGrant{JobID: 1, Kind: engine.KindWork})
	b, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "tool_grants") {
		t.Errorf("a grantless job grant carries tool_grants: %s", b)
	}

	granted := jobGrantOf(&engine.LeaseGrant{JobID: 2, Kind: engine.KindWork, ToolGrants: []string{"Bash(git merge --no-commit:*)"}})
	b, err = json.Marshal(granted)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"tool_grants":["Bash(git merge --no-commit:*)"]`) {
		t.Errorf("a granted job grant lacks tool_grants: %s", b)
	}
}
