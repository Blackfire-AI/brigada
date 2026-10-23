package claude

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
		{"cannot connect", "Unable to connect to API: Cannot connect to API: getaddrinfo ENOTFOUND api.example.invalid", adapter.ErrInfra, true},
		{"dns", "fatal: unable to access 'https://example.invalid/': Could not resolve host: example.invalid", adapter.ErrInfra, true},
		{"refused", "connect tcp 192.0.2.10:443: connection refused", adapter.ErrInfra, true},
		{"reset", "read tcp 192.0.2.10:51234->198.51.100.1:443: read: Connection reset by peer", adapter.ErrInfra, true},
		{"result error", `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Cannot connect to API: getaddrinfo ENOTFOUND api.example.invalid"}`, adapter.ErrInfra, true},
		{"auth wins", "Invalid API key: please run /login", adapter.ErrAuth, true},
		{"chatter", "MCP server example connected in 120ms", "", false},
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
