package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

type stubReview struct {
	repo, branch string
	res          engine.ReviewBranchResult
	err          error
}

func (s *stubReview) ReviewBranch(_ context.Context, repo, branch, _ string) (engine.ReviewBranchResult, error) {
	s.repo, s.branch = repo, branch
	return s.res, s.err
}

func TestReviewSubmitsAndPrintsTheJobID(t *testing.T) {
	stub := &stubReview{res: engine.ReviewBranchResult{Answer: engine.AnsCreated, JobID: 42}}
	var out, errb bytes.Buffer
	if code := printReview(context.Background(), stub, "https://example.invalid/r.git", "feature/x", &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if stub.repo != "https://example.invalid/r.git" || stub.branch != "feature/x" {
		t.Errorf("client called with %q %q", stub.repo, stub.branch)
	}
	if got := out.String(); !strings.Contains(got, "review job 42 queued for feature/x") {
		t.Errorf("stdout = %q, want the job id", got)
	}

	out.Reset()
	stub.res = engine.ReviewBranchResult{Answer: engine.AnsPending, JobID: 41}
	if code := printReview(context.Background(), stub, "", "feature/x", &out, &errb); code != 0 || !strings.Contains(out.String(), "review job 41 already") {
		t.Errorf("pending: exit %d, stdout %q", code, out.String())
	}

	stub.err = errors.New("no eligible reviewer")
	if code := printReview(context.Background(), stub, "", "feature/x", &out, &errb); code != 1 || !strings.Contains(errb.String(), "no eligible reviewer") {
		t.Errorf("error: exit %d, stderr %q", code, errb.String())
	}
}

func TestReviewUsage(t *testing.T) {
	for _, args := range [][]string{{"review"}, {"review", "feature/x"}, {"review", "--branch", "x", "extra"}} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage: brigada review --branch") {
			t.Errorf("%v: exit %d, want 2; stderr %q", args, code, errb.String())
		}
	}
	var out, errb bytes.Buffer
	run(context.Background(), []string{"help"}, &out, &errb)
	if !strings.Contains(out.String(), "review --branch") {
		t.Error("usage does not list the review command")
	}
}
