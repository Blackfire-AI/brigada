package qwencode

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
		{"cannot connect", "Error: Cannot connect to API", adapter.ErrInfra, true},
		{"dns", "Error: could not resolve host api.example.invalid", adapter.ErrInfra, true},
		{"refused", "Error: connect tcp 192.0.2.10:443: connection refused", adapter.ErrInfra, true},
		{"limit wins", "Error: too many requests, connection reset", adapter.ErrLimit, true},
		{"unknown text", "context deadline exceeded", "", false},
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
