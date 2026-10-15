package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
)

type stubPlan struct {
	approved, rejected int64
	res                api.PlanDecisionResult
	err                error
}

func (s *stubPlan) ApprovePlan(_ context.Context, id int64) (api.PlanDecisionResult, error) {
	s.approved = id
	return s.res, s.err
}

func (s *stubPlan) RejectPlan(_ context.Context, id int64) (api.PlanDecisionResult, error) {
	s.rejected = id
	return s.res, s.err
}

func TestPlanApproveAndReject(t *testing.T) {
	stub := &stubPlan{res: api.PlanDecisionResult{PlanID: 4, State: "active",
		Tasks: []api.PlanCreatedTask{{TaskID: 7, JobID: 11}}}}
	var out, errb bytes.Buffer
	if code := printPlanDecision(context.Background(), stub, "approve", 4, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if stub.approved != 4 || !strings.Contains(out.String(), "plan #4 approved: 1 tasks queued") || !strings.Contains(out.String(), "task #7: work job 11") {
		t.Errorf("approve: called with %d, stdout %q", stub.approved, out.String())
	}
	out.Reset()
	if code := printPlanDecision(context.Background(), stub, "reject", 4, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if stub.rejected != 4 || !strings.Contains(out.String(), "plan #4 rejected") {
		t.Errorf("reject: called with %d, stdout %q", stub.rejected, out.String())
	}
	stub.err = errors.New("plan is not awaiting approval")
	if code := printPlanDecision(context.Background(), stub, "approve", 4, &out, &errb); code != 1 {
		t.Errorf("exit %d on a client error, want 1", code)
	}
}

func TestPlanUsage(t *testing.T) {
	for _, args := range [][]string{{"plan"}, {"plan", "frob"}, {"plan", "approve"}, {"plan", "reject", "x"}, {"plan", "approve", "0"}, {"plan", "approve", "1", "2"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr %q", args, code, errb.String())
		}
	}
}
