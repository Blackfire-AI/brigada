package opencode

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestInfraErrorKind(t *testing.T) {
	tests := []struct {
		name   string
		status int
		msg    string
		want   adapter.ErrorKind
	}{
		{"cannot connect", 0, "Cannot connect to API: getaddrinfo ENOTFOUND api.example.invalid", adapter.ErrInfra},
		{"refused", 0, "connect tcp 192.0.2.10:443: connection refused", adapter.ErrInfra},
		{"limit wins", 429, "connection reset", adapter.ErrLimit},
		{"plain failure", 500, "internal server error", adapter.ErrOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e errorJSON
			e.Data.Message = tt.msg
			e.Data.StatusCode = tt.status
			if got := errorKind(e); got != tt.want {
				t.Fatalf("errorKind(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}
