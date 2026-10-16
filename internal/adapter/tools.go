package adapter

import (
	"fmt"
	"strings"
)

// ParseToolScope parses a Claude worker tool scope in its flag, config and
// project-file spelling: Claude Code tool patterns separated by "+", or
// "none" for the file tools alone. The result is non-nil for none, so a
// caller can tell an explicit none from no scope at all. A pattern
// containing a comma cannot ride an --agent value, which splits on commas.
func ParseToolScope(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "none" {
		return []string{}, nil
	}
	out := []string{}
	for _, f := range strings.Split(value, "+") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("want Claude Code tool patterns separated by +, or none")
	}
	return out, nil
}
