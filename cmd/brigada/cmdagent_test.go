package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type stubReset struct {
	harness, model string
	n              int
	err            error
}

func (s *stubReset) ResetAgent(_ context.Context, harness, model string) (int, error) {
	s.harness, s.model = harness, model
	return s.n, s.err
}

func TestAgentResetPrintsWhatMatched(t *testing.T) {
	stub := &stubReset{n: 2}
	var out, errb bytes.Buffer
	if code := printAgentReset(context.Background(), stub, "claude", "sonnet", &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if stub.harness != "claude" || stub.model != "sonnet" {
		t.Errorf("client called with %s/%s, want claude/sonnet", stub.harness, stub.model)
	}
	if !strings.Contains(out.String(), "claude/sonnet: reset (2 matched)") {
		t.Errorf("stdout = %q", out.String())
	}
	stub.err = errors.New("no such agent")
	if code := printAgentReset(context.Background(), stub, "x", "y", &out, &errb); code != 1 {
		t.Errorf("exit %d on a client error, want 1", code)
	}
}

func TestAgentResetUsage(t *testing.T) {
	for _, args := range [][]string{{"agent"}, {"agent", "frob"}, {"agent", "reset"}, {"agent", "reset", "claude"}, {"agent", "reset", "/x"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr %q", args, code, errb.String())
		}
	}
}
