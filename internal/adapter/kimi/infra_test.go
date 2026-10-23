package kimi

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestInfraClassification(t *testing.T) {
	a := &Adapter{}
	tests := []struct {
		name string
		line string
		want adapter.ErrorKind
		ok   bool
	}{
		{"cannot connect", "Error: failed to run prompt: Cannot connect to API", adapter.ErrInfra, true},
		{"dns", "Error: failed to run prompt: could not resolve host api.example.invalid", adapter.ErrInfra, true},
		{"refused", "Error: failed to run prompt: connect tcp 192.0.2.10:443: connection refused", adapter.ErrInfra, true},
		{"limit wins", "Error: failed to run prompt: 429 rate limit, connection reset", adapter.ErrLimit, true},
		{"plain failure", "Error: failed to run prompt: context deadline exceeded", adapter.ErrOther, true},
		{"not a failure line", "the proxy answered connection refused earlier", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, ok := a.ParseErrEvent([]byte(tt.line))
			if ok != tt.ok || (ok && ev.ErrorKind != tt.want) {
				t.Fatalf("ParseErrEvent(%q) = kind %q, ok %v; want %q, %v", tt.line, ev.ErrorKind, ok, tt.want, tt.ok)
			}
		})
	}
}
