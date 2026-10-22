package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// stubLand and stubPush stand in for the API client in the gate command
// tests (§6.3).
type stubLand struct {
	approved, rejected int64
	stopAsking         bool
	res                engine.LandDecision
	err                error
}

func (s *stubLand) LandApprove(_ context.Context, id int64, andStopAsking bool) (engine.LandDecision, error) {
	s.approved = id
	s.stopAsking = andStopAsking
	return s.res, s.err
}

func (s *stubLand) LandReject(_ context.Context, id int64) (engine.LandDecision, error) {
	s.rejected = id
	return s.res, s.err
}

type stubPush struct {
	repo string
	res  engine.PushApproveResult
	err  error
}

func (s *stubPush) PushApprove(_ context.Context, repoURL string) (engine.PushApproveResult, error) {
	s.repo = repoURL
	return s.res, s.err
}

func TestLandDecision(t *testing.T) {
	stub := &stubLand{res: engine.LandDecision{TaskID: 4, Released: true}}
	var out, errb bytes.Buffer
	if code := printLandDecision(context.Background(), stub, "approve", 4, false, &out, &errb); code != 0 {
		t.Fatalf("approve: exit %d, stderr %q", code, errb.String())
	}
	if stub.approved != 4 || !strings.Contains(out.String(), "landing for task #4 approved") {
		t.Errorf("approve: called with %d, stdout %q", stub.approved, out.String())
	}
	if stub.stopAsking {
		t.Error("approve without the flag asked the client to stop asking")
	}
	out.Reset()
	if code := printLandDecision(context.Background(), stub, "reject", 4, false, &out, &errb); code != 0 {
		t.Fatalf("reject: exit %d, stderr %q", code, errb.String())
	}
	if stub.rejected != 4 || !strings.Contains(out.String(), "landing for task #4 cancelled") {
		t.Errorf("reject: called with %d, stdout %q", stub.rejected, out.String())
	}
	stub.err = errors.New("task 4 is not held at the land gate")
	if code := printLandDecision(context.Background(), stub, "approve", 4, false, &out, &errb); code != 1 {
		t.Errorf("exit %d on a client error, want 1", code)
	}
}

// TestLandApproveStopAsking: the flag reaches the client and the changed
// rules print, one line each (§6.3).
func TestLandApproveStopAsking(t *testing.T) {
	stub := &stubLand{res: engine.LandDecision{TaskID: 6, Released: true,
		Settings: []string{"approval_tiers=", "gate_land=auto"}}}
	var out, errb bytes.Buffer
	if code := printLandDecision(context.Background(), stub, "approve", 6, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if !stub.stopAsking {
		t.Error("the flag did not reach the client")
	}
	for _, want := range []string{"rule changed: approval_tiers=", "rule changed: gate_land=auto"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q lacks %q", out.String(), want)
		}
	}
}

func TestPushApprove(t *testing.T) {
	stub := &stubPush{res: engine.PushApproveResult{Released: 2}}
	var out, errb bytes.Buffer
	if code := printPushApprove(context.Background(), stub, "https://example.invalid/one.git", &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if stub.repo != "https://example.invalid/one.git" || !strings.Contains(out.String(), "2 landing(s) queued to push") {
		t.Errorf("push approve: called with %q, stdout %q", stub.repo, out.String())
	}
	out.Reset()
	stub.res = engine.PushApproveResult{}
	if code := printPushApprove(context.Background(), stub, "https://example.invalid/one.git", &out, &errb); code != 0 || !strings.Contains(out.String(), "nothing waits to push") {
		t.Errorf("empty push approve: exit %d, stdout %q", code, out.String())
	}
}

// TestLandDispatchesDecisionSubcommands: `land approve` and `land reject`
// route to the decision commands, while a plain `land <id>` still reaches
// the hand-driven form (§7.1) — proven by its own usage error, not by a
// git run.
func TestLandDispatchesDecisionSubcommands(t *testing.T) {
	for _, args := range [][]string{
		{"land", "approve"}, {"land", "approve", "x"}, {"land", "approve", "0"},
		{"land", "reject"}, {"land", "reject", "1", "2"},
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr %q", args, code, errb.String())
		}
	}
	// The hand-driven form still runs: it fails at run time, not with a
	// usage error. It must never reach a real engine or repository: from
	// the package directory a real land's check reruns this test, so it
	// runs outside any repository, with its own state and config and no
	// engine started.
	t.Chdir(t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"land", "--no-start", "1"}, &out, &errb); code != 1 {
		t.Errorf("land 1: exit %d, want 1 (no engine, no repository); stderr %q", code, errb.String())
	}
}

// TestPushUsage: the push command needs a subcommand.
func TestPushUsage(t *testing.T) {
	for _, args := range [][]string{{"push"}, {"push", "frob"}, {"push", "approve", "extra"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr %q", args, code, errb.String())
		}
	}
}
