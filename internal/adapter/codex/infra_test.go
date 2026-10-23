package codex

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
		{"dns", "stream error: error sending request for url (https://example.invalid/v1/responses): dns error: could not resolve host", adapter.ErrInfra, true},
		{"refused", "stream disconnected: connect tcp 192.0.2.10:443: connection refused", adapter.ErrInfra, true},
		{"reset", "read tcp 192.0.2.10:51234->198.51.100.1:443: read: Connection reset by peer", adapter.ErrInfra, true},
		{"json error", `{"type":"error","message":"Cannot connect to API: could not resolve host"}`, adapter.ErrInfra, true},
		{"auth wins", "Not logged in", adapter.ErrAuth, true},
		{"chatter", "reading prompt from stdin", "", false},
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
