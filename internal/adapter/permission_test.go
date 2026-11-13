package adapter

import (
	"strings"
	"testing"
)

func TestValidatePermission(t *testing.T) {
	for _, ok := range []string{"bypassPermissions", "acceptEdits", "plan", "none"} {
		if err := ValidatePermission(ok); err != nil {
			t.Errorf("ValidatePermission(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a,b", "a\tb", "a\nb", "a\x00b"} {
		if err := ValidatePermission(bad); err == nil {
			t.Errorf("ValidatePermission(%q) accepted", bad)
		}
	}
}

func TestToolsAliasPermission(t *testing.T) {
	if got := ToolsAliasPermission([]string{}); got != "none" {
		t.Errorf("empty scope = %q, want none", got)
	}
	if got := ToolsAliasPermission(nil); got != "none" {
		t.Errorf("nil scope = %q, want none", got)
	}
	if got := ToolsAliasPermission([]string{"Bash(go test:*)", "Bash(gofmt:*)"}); got != "Bash(go test:*)+Bash(gofmt:*)" {
		t.Errorf("patterns = %q", got)
	}
}

func TestDefaultPermission(t *testing.T) {
	if got := DefaultPermission("claude"); got != "bypassPermissions" {
		t.Errorf("claude = %q, want bypassPermissions", got)
	}
	for _, h := range []string{"codex", "kimi", "opencode", "dsh", "qwen-code", "fake"} {
		if got := DefaultPermission(h); got != "" {
			t.Errorf("%s = %q, want empty", h, got)
		}
	}
}

func TestDeprecatedKeyWarning(t *testing.T) {
	w := DeprecatedKeyWarning("worker_tools")
	if !strings.Contains(w, "worker_tools") || !strings.Contains(w, "permission=") {
		t.Errorf("warning = %q, want the key and permission=", w)
	}
}
