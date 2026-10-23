package dsh

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestInfraErrorKind(t *testing.T) {
	tests := []struct {
		name string
		e    errorJSON
		want adapter.ErrorKind
	}{
		{"cannot connect", errorJSON{Message: "Cannot connect to API"}, adapter.ErrInfra},
		{"dns", errorJSON{Message: "could not resolve host api.example.invalid"}, adapter.ErrInfra},
		{"reset", errorJSON{Message: "read: Connection reset by peer"}, adapter.ErrInfra},
		{"auth wins", errorJSON{Code: "AUTH", Message: "connection refused"}, adapter.ErrAuth},
		{"plain failure", errorJSON{Message: "model overloaded"}, adapter.ErrOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorKind(tt.e); got != tt.want {
				t.Fatalf("errorKind(%+v) = %q, want %q", tt.e, got, tt.want)
			}
		})
	}
}
