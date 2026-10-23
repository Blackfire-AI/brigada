package testsupport

import "testing"

func TestParseProcStat(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		state byte
		pgrp  int
		ok    bool
	}{
		{"sleeping", "1234 (sleep) S 1 4321 1234 0 -1 4194560", 'S', 4321, true},
		{"zombie", "77 (engine) Z 1 5002 5002 0 -1 4194560", 'Z', 5002, true},
		{"comm with spaces and parens", "88 (a (b) c) S 1 99 99 0 -1", 'S', 99, true},
		{"comm with a state lookalike", "5 (x) Z) S 1 42 42 0", 'S', 42, true},
		{"no paren", "garbage", 0, 0, false},
		{"truncated", "5 (x) S 1", 0, 0, false},
		{"bad pgrp", "5 (x) S 1 abc", 0, 0, false},
	}
	for _, tt := range tests {
		state, pgrp, ok := ParseProcStat([]byte(tt.line))
		if state != tt.state || pgrp != tt.pgrp || ok != tt.ok {
			t.Errorf("%s: got (%q, %d, %v), want (%q, %d, %v)", tt.name, state, pgrp, ok, tt.state, tt.pgrp, tt.ok)
		}
	}
}
