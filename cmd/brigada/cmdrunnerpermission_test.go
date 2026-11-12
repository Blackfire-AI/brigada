package main

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
)

// TestAgentSpecPermission: permission= parses and is stored, tools= is its
// deprecated alias with a warning, read-scope= defaults to on and turns off.
func TestAgentSpecPermission(t *testing.T) {
	s := &agentSpecs{}
	for _, v := range []string{
		"harness=claude,permission=acceptEdits,read-scope=false",
		"harness=claude,model=opus,tools=Bash(go test:*)",
		"harness=claude,model=haiku",
		"harness=claude,model=fable,permission=none,tools=none",
	} {
		if err := s.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	file := lifecycle.FileConfig{}

	perm, scope := s.specs[0].resolve(file)
	if perm != "acceptEdits" || scope {
		t.Errorf("explicit: permission %q readScope %v, want acceptEdits and off", perm, scope)
	}
	if w := s.specs[0].warnings(); len(w) != 0 {
		t.Errorf("permission= warned: %v", w)
	}
	if got := s.specs[0].String(); !strings.Contains(got, "permission=acceptEdits") || !strings.Contains(got, "read-scope=false") {
		t.Errorf("String() = %q, want permission and read-scope", got)
	}

	perm, scope = s.specs[1].resolve(file)
	if perm != "Bash(go test:*)" || !scope {
		t.Errorf("tools alias: permission %q readScope %v", perm, scope)
	}
	if w := s.specs[1].warnings(); len(w) != 1 || !strings.Contains(w[0], "tools") || !strings.Contains(w[0], "permission=") {
		t.Errorf("tools= warnings = %v, want one naming tools and permission=", w)
	}

	perm, scope = s.specs[2].resolve(file)
	if perm != "bypassPermissions" || !scope {
		t.Errorf("default: permission %q readScope %v, want bypassPermissions and on", perm, scope)
	}

	if perm, _ = s.specs[3].resolve(file); perm != "none" {
		t.Errorf("equal permission and tools = %q, want none", perm)
	}
}

func TestAgentSpecPermissionFromFile(t *testing.T) {
	file := lifecycle.FileConfig{
		Permission: map[string]string{"claude/sonnet": "plan"},
		ReadScope:  map[string]bool{"claude/sonnet": false},
	}
	s := &agentSpecs{}
	if err := s.Set("harness=claude"); err != nil {
		t.Fatal(err)
	}
	if perm, scope := s.specs[0].resolve(file); perm != "plan" || scope {
		t.Errorf("file: permission %q readScope %v, want plan and off", perm, scope)
	}
	if err := s.Set("harness=claude,read-scope=true,permission=default"); err != nil {
		t.Fatal(err)
	}
	if perm, scope := s.specs[1].resolve(file); perm != "default" || !scope {
		t.Errorf("flag over file: permission %q readScope %v, want default and on", perm, scope)
	}
}

func TestAgentSpecPermissionRefusals(t *testing.T) {
	for _, v := range []string{
		"harness=claude,permission=",
		"harness=claude,permission=a b",
		"harness=claude,read-scope=maybe",
		"harness=claude,permission=plan,tools=none",
	} {
		if err := (&agentSpecs{}).Set(v); err == nil {
			t.Errorf("Set(%q) accepted", v)
		}
	}
}
