package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestMCPSelectorsMutuallyExclusive(t *testing.T) {
	var out, errout bytes.Buffer
	if code := runMCP(context.Background(), []string{"--coordinator", "--client"}, &out, &errout); code != 2 || !strings.Contains(errout.String(), "mutually exclusive") {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
}

// TestMCPForemanSelectorIsRetired: mode A is gone, so `brigada mcp
// --foreman` is a usage error, not a bridge for the user's own session.
func TestMCPForemanSelectorIsRetired(t *testing.T) {
	var out, errout bytes.Buffer
	if code := runMCP(context.Background(), []string{"--foreman"}, &out, &errout); code != 2 || !strings.Contains(errout.String(), "-foreman") {
		t.Fatalf("code=%d stderr=%s", code, errout.String())
	}
}
