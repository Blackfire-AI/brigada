package adapter

import (
	"fmt"
	"strings"
)

// DefaultClaudePermission is the permission mode a claude agent runs under
// when it sets none (§5.2): a headless run cannot answer an ask.
const DefaultClaudePermission = "bypassPermissions"

// DefaultPermission is the permission mode an agent of the harness takes
// when it sets no permission=. Only claude has a default; the value of any
// other harness is empty, which its adapter reads as its own behaviour.
func DefaultPermission(harness string) string {
	if harness == "claude" {
		return DefaultClaudePermission
	}
	return ""
}

// ValidatePermission checks a permission= value as brigada stores it: a
// non-empty token without spaces, commas or control characters. Whether
// the harness knows the mode is the adapter's to say; nothing here does.
func ValidatePermission(v string) error {
	if v == "" {
		return fmt.Errorf("want a permission mode, got an empty value")
	}
	if strings.ContainsAny(v, " \t\r\n,") || strings.ContainsFunc(v, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return fmt.Errorf("want one permission mode without spaces or commas, got %q", v)
	}
	return nil
}

// ToolsAliasPermission is the permission value a deprecated tools= or
// worker_tools scope maps onto: the scope in its tools= spelling, patterns
// joined by "+", and "none" for the explicitly empty scope.
func ToolsAliasPermission(tools []string) string {
	if len(tools) == 0 {
		return "none"
	}
	return strings.Join(tools, "+")
}

// DeprecatedKeyWarning is the warning for a use of a deprecated alias of
// permission=; it names the key and its replacement.
func DeprecatedKeyWarning(key string) string {
	return fmt.Sprintf("%s is deprecated; use permission= instead (%s is kept as an alias until the next major version)", key, key)
}
