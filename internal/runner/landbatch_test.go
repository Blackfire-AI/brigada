package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The batch run (§7.1 "Running a batch", "Acceptance outcomes") against a
// local bare remote: a jobRun holding a batch grant lands its members in
// a clone under a temporary state directory, the engine's side answered
// by a client whose every call is ok.

// batchMember pushes one member's result branch to the remote — the
// files, on top of base — and returns its grant entry, approved at the
// branch's head.
func batchMember(t *testing.T, remote string, task int64, base string, deps []int64, files map[string]string) engine.LandMember {
	t.Helper()
	scratch := t.TempDir()
	git(t, "", "clone", "-q", "--branch", base, remote, scratch)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(scratch, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, scratch, "add", "-A")
	git(t, scratch, "-c", "user.name=w", "-c", "user.email=w@brigada.invalid", "commit", "-qm", fmt.Sprintf("task %d", task))
	branch := fmt.Sprintf("brigada/task-%d", task)
	git(t, scratch, "push", "-q", "origin", "HEAD:refs/heads/"+branch)
	return engine.LandMember{
		TaskID:       task,
		Branch:       branch,
		BaseRef:      base,
		ApprovedHead: strings.TrimSpace(git(t, scratch, "rev-parse", "HEAD")),
		Deps:         deps,
		Message:      &engine.LandMessage{Title: fmt.Sprintf("task %d", task), Spec: "the spec", Summary: "done"},
	}
}

// batchRun is a jobRun holding a batch grant of members on remote's
// main, its clone prepared.
func batchRun(t *testing.T, remote, gate, check string, members ...engine.LandMember) (*jobRun, string) {
	t.Helper()
	client := api.NewClientWithHTTPClient(&http.Client{Transport: usageTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"answer":"ok"}`)), Header: make(http.Header)}, nil
	})}, "test-token")
	var identity []string
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		identity = append(identity, k+"_NAME=land test", k+"_EMAIL=land@brigada.invalid")
	}
	r := &jobRun{
		cfg: &Config{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), GitEnv: identity,
			Clock: realClock(), landRetryPause: func(int) time.Duration { return time.Millisecond }},
		client: client,
		grant: &api.JobGrant{JobID: 7, LeaseID: "lease", TaskID: members[0].TaskID, Kind: string(engine.KindLand),
			RepoURL: remote, LandTarget: "main", DefaultBranch: "main", GatePush: gate, CheckCmd: check,
			LandBatch: true, LandMembers: members},
	}
	dir := landCloneDir(r.cfg.StateDir, r.grant)
	if err := r.prepareLandClone(context.Background(), dir, remote); err != nil {
		t.Fatal(err)
	}
	return r, dir
}

// outcomes words each settled member as "<task>:<outcome>".
func outcomes(settled map[int64]landMemberJSON, members []engine.LandMember) []string {
	var out []string
	for _, m := range members {
		s, ok := settled[m.TaskID]
		if !ok {
			out = append(out, fmt.Sprintf("%d:-", m.TaskID))
			continue
		}
		out = append(out, fmt.Sprintf("%d:%s", m.TaskID, s.Outcome))
	}
	return out
}

func wantOutcomes(t *testing.T, settled map[int64]landMemberJSON, members []engine.LandMember, want ...string) {
	t.Helper()
	if got := outcomes(settled, members); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
}

// commitCount counts the commits on the remote's ref.
func commitCount(t *testing.T, remote, ref string) string {
	t.Helper()
	return strings.TrimSpace(git(t, "", "--git-dir", strings.TrimPrefix(remote, "file://"), "rev-list", "--count", ref))
}

// TestLandBatchMiddleConflictParksAlone: the middle member's diff
// conflicts with the target, so it parks alone with its files, its
// dependent in the batch drops to wait, and the independent members land
// in one push, each its own commit in batch order.
func TestLandBatchMiddleConflictParksAlone(t *testing.T) {
	remote := newRemote(t)
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	b := batchMember(t, remote, 2, "main", nil, map[string]string{"README.md": "# b's seed\n"})
	c := batchMember(t, remote, 3, b.Branch, []int64{2}, map[string]string{"c.txt": "c\n"})
	d := batchMember(t, remote, 4, "main", nil, map[string]string{"d.txt": "d\n"})
	moved := pushToMain(t, remote, "README.md", "# another engineer's seed\n")
	members := []engine.LandMember{a, b, c, d}
	r, dir := batchRun(t, remote, engine.GateAuto, "", members...)

	settled, errMsg, _ := batchLoop(r, dir)
	if errMsg != "" {
		t.Fatalf("the batch stopped: %s", errMsg)
	}
	wantOutcomes(t, settled, members, "1:landed", "2:parked", "3:dropped", "4:landed")
	if files := settled[2].Record.ConflictFiles; len(files) != 1 || files[0] != "README.md" {
		t.Errorf("the conflict's files = %v, want README.md", files)
	}
	if settled[3].BlockedBy != 2 || settled[3].Record != nil {
		t.Errorf("the dependent = %+v, want dropped behind task 2 with nothing recorded", settled[3])
	}
	top := settled[4].Record.Commit
	if got := remoteBranchCommit(t, remote, "main"); got != top {
		t.Errorf("main = %s, want the last surviving member %s", got, top)
	}
	if got := parentOf(t, remote, top); got != settled[1].Record.Commit {
		t.Errorf("task 4's commit sits on %s, want task 1's %s", got, settled[1].Record.Commit)
	}
	if got := parentOf(t, remote, settled[1].Record.Commit); got != moved {
		t.Errorf("task 1's commit sits on %s, want the moved target %s", got, moved)
	}
	for _, id := range []int64{1, 4} {
		if p := settled[id].Record.Pushed; p != "pushed" {
			t.Errorf("task %d pushed = %q, want pushed", id, p)
		}
	}
}

// TestLandBatchCheckFailureParksTheMemberJustAdded: the check fails on
// the stack through one member's commit, so that member parks with its
// check output, its attempt discarded; its dependent drops, and the
// independent member lands on the preceding passing stack.
func TestLandBatchCheckFailureParksTheMemberJustAdded(t *testing.T) {
	remote := newRemote(t)
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	b := batchMember(t, remote, 2, "main", nil, map[string]string{"bad.txt": "bad\n"})
	c := batchMember(t, remote, 3, b.Branch, []int64{2}, map[string]string{"c.txt": "c\n"})
	d := batchMember(t, remote, 4, "main", nil, map[string]string{"d.txt": "d\n"})
	members := []engine.LandMember{a, b, c, d}
	r, dir := batchRun(t, remote, engine.GateAuto, `test ! -e bad.txt || { echo "bad.txt is here"; exit 1; }`, members...)

	settled, errMsg, _ := batchLoop(r, dir)
	if errMsg != "" {
		t.Fatalf("the batch stopped: %s", errMsg)
	}
	wantOutcomes(t, settled, members, "1:landed", "2:parked", "3:dropped", "4:landed")
	if rec := settled[2].Record; rec == nil || rec.CheckPassed || !strings.Contains(rec.CheckLog, "bad.txt is here") {
		t.Errorf("the failed member's record = %+v, want its check output", rec)
	}
	if got := parentOf(t, remote, settled[4].Record.Commit); got != settled[1].Record.Commit {
		t.Errorf("task 4 sits on %s, want the preceding passing stack %s", got, settled[1].Record.Commit)
	}
	log, err := os.ReadFile(filepath.Join(r.cfg.StateDir, "records", "job-7-task-2-check.log"))
	if err != nil {
		t.Fatalf("the member's check artifact: %v", err)
	}
	if !strings.HasPrefix(string(log), "land batch job 7: task 2, commit ") {
		t.Errorf("the check artifact begins %q, want it to name the member and commit", firstLineOf(string(log)))
	}
}

// TestLandBatchAskWaitsInLandingOrder: under ask the survivors wait on
// the waiting ref, the last at its tip, each on the one before, and the
// target is untouched; under never the same, each with the exact push
// command.
func TestLandBatchAskWaitsInLandingOrder(t *testing.T) {
	for _, gate := range []string{engine.GateAsk, engine.GateNever} {
		t.Run(gate, func(t *testing.T) {
			remote := newRemote(t)
			seed := remoteBranchCommit(t, remote, "main")
			a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
			b := batchMember(t, remote, 2, a.Branch, []int64{1}, map[string]string{"b.txt": "b\n"})
			c := batchMember(t, remote, 3, "main", nil, map[string]string{"c.txt": "c\n"})
			members := []engine.LandMember{a, b, c}
			r, dir := batchRun(t, remote, gate, "", members...)

			settled, errMsg, _ := batchLoop(r, dir)
			if errMsg != "" {
				t.Fatalf("the batch stopped: %s", errMsg)
			}
			wantOutcomes(t, settled, members, "1:landed", "2:landed", "3:landed")
			if got := remoteBranchCommit(t, remote, "main"); got != seed {
				t.Errorf("main moved under %s: %s, want the seed", gate, got)
			}
			if got := remoteRefCommit(t, remote, landWaitRef("main")); got != settled[3].Record.Commit {
				t.Errorf("the waiting ref = %s, want the last member %s", got, settled[3].Record.Commit)
			}
			prev := seed
			for _, id := range []int64{1, 2, 3} {
				rec := settled[id].Record
				if got := parentOf(t, remote, rec.Commit); got != prev {
					t.Errorf("task %d sits on %s, want %s: the stack keeps landing order", id, got, prev)
				}
				prev = rec.Commit
				want := "waiting"
				if gate == engine.GateNever {
					want = "local"
					if rec.PushCmd != landPushCmd("main") {
						t.Errorf("task %d push command = %q", id, rec.PushCmd)
					}
				}
				if rec.Pushed != want {
					t.Errorf("task %d pushed = %q, want %q", id, rec.Pushed, want)
				}
			}
		})
	}
}

// TestLandBatchEnvironmentFailureStopsTheBatch: a push the transport
// cannot carry stops the batch with an environment error — every
// locally checked member is left to the job's error, none recorded —
// while a member already parked for its content keeps its own failure
// and its dependent stays dropped.
func TestLandBatchEnvironmentFailureStopsTheBatch(t *testing.T) {
	remote := newRemote(t)
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"README.md": "# a's seed\n"})
	b := batchMember(t, remote, 2, a.Branch, []int64{1}, map[string]string{"b.txt": "b\n"})
	c := batchMember(t, remote, 3, "main", nil, map[string]string{"c.txt": "c\n"})
	pushToMain(t, remote, "README.md", "# another engineer's seed\n")
	members := []engine.LandMember{a, b, c}
	r, dir := batchRun(t, remote, engine.GateAuto, "", members...)
	git(t, dir, "config", "remote.origin.pushurl", filepath.Join(t.TempDir(), "gone.git"))

	settled, errMsg, _ := batchLoop(r, dir)
	if errMsg == "" {
		t.Fatal("the batch did not stop on the unreachable push")
	}
	if !strings.Contains(strings.ToLower(errMsg), "could not read from remote repository") {
		t.Errorf("the batch error = %q, want git's transport failure", errMsg)
	}
	wantOutcomes(t, settled, members, "1:parked", "2:dropped", "3:-")
}

// TestLandBatchLostRunnerResumes: a runner lost before its push leaves
// nothing on the remote, and the next lease lands every member once; a
// runner lost after its push is recovered from the remote — every member
// recognized by its trailers, recorded with its own commit, nothing
// squashed again.
func TestLandBatchLostRunnerResumes(t *testing.T) {
	remote := newRemote(t)
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	b := batchMember(t, remote, 2, a.Branch, []int64{1}, map[string]string{"b.txt": "b\n"})
	members := []engine.LandMember{a, b}

	// Lost before publication: the push never happened.
	r, dir := batchRun(t, remote, engine.GateAuto, "", members...)
	git(t, dir, "config", "remote.origin.pushurl", filepath.Join(t.TempDir(), "gone.git"))
	if _, errMsg, _ := batchLoop(r, dir); errMsg == "" {
		t.Fatal("the first lease published")
	}
	if got := commitCount(t, remote, "main"); got != "1" {
		t.Fatalf("commits on main after the lost lease = %s, want the seed alone", got)
	}

	// The next lease lands both once.
	r, dir = batchRun(t, remote, engine.GateAuto, "", members...)
	first, errMsg, _ := batchLoop(r, dir)
	if errMsg != "" {
		t.Fatalf("the second lease stopped: %s", errMsg)
	}
	wantOutcomes(t, first, members, "1:landed", "2:landed")
	tip := remoteBranchCommit(t, remote, "main")

	// Lost after publication: a third lease finds both on the target.
	r, dir = batchRun(t, remote, engine.GateAuto, "", members...)
	again, errMsg, _ := batchLoop(r, dir)
	if errMsg != "" {
		t.Fatalf("the recovering lease stopped: %s", errMsg)
	}
	wantOutcomes(t, again, members, "1:landed", "2:landed")
	for _, id := range []int64{1, 2} {
		if again[id].Record.Commit != first[id].Record.Commit || again[id].Record.Pushed != "pushed" {
			t.Errorf("task %d recovered as %+v, want its published commit %s pushed", id, again[id].Record, first[id].Record.Commit)
		}
	}
	if got := remoteBranchCommit(t, remote, "main"); got != tip {
		t.Errorf("main moved on recovery: %s, want %s", got, tip)
	}
	if got := commitCount(t, remote, "main"); got != "3" {
		t.Errorf("commits on main = %s, want the seed and each member once", got)
	}
}

// TestLandBatchRejectedPushRetriesTheRemainder: a rejected push discards
// the unpublished attempt and lands every remaining member from a fresh
// fetch, the check rerun, in one push.
func TestLandBatchRejectedPushRetriesTheRemainder(t *testing.T) {
	markerDir(t)
	remote := landHookRemote(t, 1)
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	b := batchMember(t, remote, 2, "main", nil, map[string]string{"b.txt": "b\n"})
	members := []engine.LandMember{a, b}
	r, dir := batchRun(t, remote, engine.GateAuto, "true", members...)

	settled, errMsg, _ := batchLoop(r, dir)
	if errMsg != "" {
		t.Fatalf("the batch stopped: %s", errMsg)
	}
	wantOutcomes(t, settled, members, "1:landed", "2:landed")
	if got := remoteBranchCommit(t, remote, "main"); got != settled[2].Record.Commit {
		t.Errorf("main = %s, want the second member %s", got, settled[2].Record.Commit)
	}
}

// TestLandBatchReconciledMembersSurviveAnError: a member a lost lease
// already published is reconciled from the remote, and stays landed when
// the rest of the attempt stops on an environment failure — never left
// to the job's error to park.
func TestLandBatchReconciledMembersSurviveAnError(t *testing.T) {
	remote := newRemote(t)
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	b := batchMember(t, remote, 2, "main", nil, map[string]string{"b.txt": "b\n"})
	r, dir := batchRun(t, remote, engine.GateAuto, "", a)
	first, errMsg, _ := batchLoop(r, dir)
	if errMsg != "" {
		t.Fatalf("the first lease stopped: %s", errMsg)
	}

	members := []engine.LandMember{a, b}
	r, dir = batchRun(t, remote, engine.GateAuto, "", members...)
	git(t, dir, "config", "remote.origin.pushurl", filepath.Join(t.TempDir(), "gone.git"))
	settled, errMsg, _ := batchLoop(r, dir)
	if errMsg == "" {
		t.Fatal("the batch did not stop on the unreachable push")
	}
	wantOutcomes(t, settled, members, "1:landed", "2:-")
	if got := settled[1].Record.Commit; got != first[1].Record.Commit {
		t.Errorf("the reconciled member's commit = %s, want its published %s", got, first[1].Record.Commit)
	}
}

// TestLandBatchLostPushResponseIsReconciled: a push that lands but
// answers with a failure is reconciled from the remote before any member
// is judged unlanded — the stack is recorded, nothing parks.
func TestLandBatchLostPushResponseIsReconciled(t *testing.T) {
	remote := newRemote(t)
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	b := batchMember(t, remote, 2, a.Branch, []int64{1}, map[string]string{"b.txt": "b\n"})
	members := []engine.LandMember{a, b}
	r, dir := batchRun(t, remote, engine.GateAuto, "", members...)
	// The clone's pre-push hook makes the push itself, then fails: the
	// remote holds the stack, the runner hears an error that names no
	// rejection.
	hook := "#!/bin/sh\nwhile read lref lsha rref rsha; do git push --quiet --no-verify origin \"$lsha:$rref\" || exit 2; done\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-push"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	settled, errMsg, _ := batchLoop(r, dir)
	if errMsg != "" {
		t.Fatalf("the batch stopped though its push landed: %s", errMsg)
	}
	wantOutcomes(t, settled, members, "1:landed", "2:landed")
	if got := remoteBranchCommit(t, remote, "main"); got != settled[2].Record.Commit {
		t.Errorf("main = %s, want the stack's tip %s", got, settled[2].Record.Commit)
	}
	if got := commitCount(t, remote, "main"); got != "3" {
		t.Errorf("commits on main = %s, want the seed and each member once", got)
	}
}

// TestLandBatchPushBoundParksTheSurvivors: a target that rejects every
// push gets five attempts in all; then each surviving member parks with
// its own rejected-push failure naming the rejection and the count,
// nothing published, and a member parked for its content keeps that.
func TestLandBatchPushBoundParksTheSurvivors(t *testing.T) {
	markers := markerDir(t)
	remote := landHookRemote(t, 100)
	seed := remoteBranchCommit(t, remote, "main")
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	b := batchMember(t, remote, 2, "main", nil, map[string]string{"bad.txt": "bad\n"})
	c := batchMember(t, remote, 3, "main", nil, map[string]string{"c.txt": "c\n"})
	members := []engine.LandMember{a, b, c}
	r, dir := batchRun(t, remote, engine.GateAuto, "test ! -e bad.txt", members...)

	settled, errMsg, _ := batchLoop(r, dir)
	if errMsg != "" {
		t.Fatalf("the batch stopped: %s", errMsg)
	}
	wantOutcomes(t, settled, members, "1:parked", "2:parked", "3:parked")
	for _, id := range []int64{1, 3} {
		if why := settled[id].Reason; !strings.Contains(why, "rejected after 5 attempts") {
			t.Errorf("task %d reason = %q, want the rejection and the count", id, why)
		}
	}
	if rec := settled[2].Record; rec == nil || rec.CheckPassed {
		t.Errorf("the content-failed member = %+v, want its own check failure kept", settled[2])
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(markers, "pushes"))); got != "5" {
		t.Errorf("pushes the target saw = %s, want five in all", got)
	}
	if got := remoteBranchCommit(t, remote, "main"); got != seed {
		t.Errorf("main moved: %s, want the seed", got)
	}
}

// TestLandBatchPushApprovalReleasesAPrefix: under ask and never the
// batch's members wait in landing order; the push phase of a middle
// landing pushes its recorded commit — carrying the earlier one, leaving
// the later one waiting — and the last landing's push follows with no
// new squash.
func TestLandBatchPushApprovalReleasesAPrefix(t *testing.T) {
	for _, gate := range []string{engine.GateAsk, engine.GateNever} {
		t.Run(gate, func(t *testing.T) {
			remote := newRemote(t)
			a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
			b := batchMember(t, remote, 2, "main", nil, map[string]string{"b.txt": "b\n"})
			c := batchMember(t, remote, 3, "main", nil, map[string]string{"c.txt": "c\n"})
			members := []engine.LandMember{a, b, c}
			r, dir := batchRun(t, remote, gate, "", members...)
			settled, errMsg, _ := batchLoop(r, dir)
			if errMsg != "" {
				t.Fatalf("the batch stopped: %s", errMsg)
			}
			wantOutcomes(t, settled, members, "1:landed", "2:landed", "3:landed")
			top := settled[3].Record.Commit

			push := func(task int64) {
				t.Helper()
				p, _ := batchRun(t, remote, gate, "", members...)
				p.grant.LandBatch, p.grant.LandMembers = false, nil
				p.grant.Push, p.grant.TaskID, p.grant.LandCommit = true, task, settled[task].Record.Commit
				p.runLandPushPhase(context.Background())
			}
			push(2)
			if got := remoteBranchCommit(t, remote, "main"); got != settled[2].Record.Commit {
				t.Errorf("main after the middle approval = %s, want its prefix %s", got, settled[2].Record.Commit)
			}
			if got := remoteRefCommit(t, remote, landWaitRef("main")); got != top {
				t.Errorf("the waiting ref = %s, want the later landing %s still waiting", got, top)
			}
			push(3)
			if got := remoteBranchCommit(t, remote, "main"); got != top {
				t.Errorf("main after the last approval = %s, want %s", got, top)
			}
			if got := commitCount(t, remote, "main"); got != "4" {
				t.Errorf("commits on main = %s, want the seed and three squashes, no new one", got)
			}
		})
	}
}

// readFile reads a file whole, failing the test when it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// batchLoop runs the batch's attempts with nothing settled before them.
func batchLoop(r *jobRun, dir string) (map[int64]landMemberJSON, string, string) {
	settled := map[int64]landMemberJSON{}
	errMsg, kind := r.landBatchLoop(context.Background(), dir, settled)
	return settled, errMsg, kind
}

// capturingClient answers every call ok and keeps each request body.
func capturingClient(bodies *[]string) *api.Client {
	return api.NewClientWithHTTPClient(&http.Client{Transport: usageTransport(func(req *http.Request) (*http.Response, error) {
		if req.Body != nil {
			b, _ := io.ReadAll(req.Body)
			*bodies = append(*bodies, string(b))
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"answer":"ok"}`)), Header: make(http.Header)}, nil
	})}, "test-token")
}

// TestLandBatchMixedGateHoldsOnlyTheUnreleased (§7.1 "Deciding a batch
// hold"): in a batch whose members' gates are partly released, the
// command runs once and its exit 1 holds only the member subject to it,
// with the command's line; the released member lands in this batch, and
// a released dependent of the newly held member drops to wait. A batch
// whose every gate is released runs no command; one with none released
// completes the job with the command's own answer.
func TestLandBatchMixedGateHoldsOnlyTheUnreleased(t *testing.T) {
	markers := markerDir(t)
	remote := landReadyRemote(t, "echo x >> '"+markers+"/ran'\necho 'CI is red'\nexit 1\n")
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	b := batchMember(t, remote, 2, "main", nil, map[string]string{"b.txt": "b\n"})
	c := batchMember(t, remote, 3, b.Branch, []int64{2}, map[string]string{"c.txt": "c\n"})
	a.SkipLandReady, c.SkipLandReady = true, true
	members := []engine.LandMember{a, b, c}
	r, dir := batchRun(t, remote, engine.GateAuto, "", members...)
	r.grant.LandReady = ".brigada/land-ready.sh"
	r.cfg.CheckTimeout = 30 * time.Second

	settled := map[int64]landMemberJSON{}
	if !r.runLandBatchReady(context.Background(), dir, settled) {
		t.Fatal("the mixed batch's gate completed the job; want the released members to go on")
	}
	if errMsg, _ := r.landBatchLoop(context.Background(), dir, settled); errMsg != "" {
		t.Fatalf("the batch stopped: %s", errMsg)
	}
	wantOutcomes(t, settled, members, "1:landed", "2:held", "3:dropped")
	if got := settled[2].Line; got != "CI is red" {
		t.Errorf("the held member's line = %q, want the command's first line", got)
	}
	if settled[3].BlockedBy != 2 {
		t.Errorf("the released dependent = %+v, want dropped behind the held member", settled[3])
	}
	if got := remoteBranchCommit(t, remote, "main"); got != settled[1].Record.Commit {
		t.Errorf("main = %s, want only the released member's commit %s", got, settled[1].Record.Commit)
	}
	if got := strings.Count(readFile(t, filepath.Join(markers, "ran")), "x"); got != 1 {
		t.Errorf("the command ran %d times, want once for the batch", got)
	}

	// Every gate released: nothing runs, nothing is settled by the gate.
	if err := os.Remove(filepath.Join(markers, "ran")); err != nil {
		t.Fatal(err)
	}
	b.SkipLandReady = true
	r2, dir2 := batchRun(t, remote, engine.GateAuto, "", a, b, c)
	r2.grant.LandReady, r2.cfg.CheckTimeout = ".brigada/land-ready.sh", 30*time.Second
	none := map[int64]landMemberJSON{}
	if !r2.runLandBatchReady(context.Background(), dir2, none) || len(none) != 0 {
		t.Errorf("an all-released batch's gate = %v, want nothing settled", none)
	}
	if _, err := os.Stat(filepath.Join(markers, "ran")); err == nil {
		t.Error("the command ran for a batch whose every gate is released")
	}

	// No gate released: the command's hold is the batch's, the job done.
	a.SkipLandReady, b.SkipLandReady, c.SkipLandReady = false, false, false
	r3, dir3 := batchRun(t, remote, engine.GateAuto, "", a, b, c)
	var bodies []string
	r3.client = capturingClient(&bodies)
	r3.grant.LandReady, r3.cfg.CheckTimeout = ".brigada/land-ready.sh", 30*time.Second
	if r3.runLandBatchReady(context.Background(), dir3, map[int64]landMemberJSON{}) {
		t.Fatal("an unreleased batch's gate let it go on; want the batch held")
	}
	if !slices.ContainsFunc(bodies, func(s string) bool { return strings.Contains(s, `"outcome":"held"`) }) {
		t.Errorf("completions %q, want the batch's held outcome", bodies)
	}
}

// TestLandRecheckLandsNothing (§7.1 "Deciding a batch hold"): the
// engine's re-check of a squash-phase hold runs the command and, on its
// yes, completes with the ready outcome — nothing squashed or pushed —
// for the engine to release the member into the next batch.
func TestLandRecheckLandsNothing(t *testing.T) {
	remote := landReadyRemote(t, "exit 0\n")
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	r, _ := batchRun(t, remote, engine.GateAuto, "", a)
	var bodies []string
	r.client = capturingClient(&bodies)
	r.grant.LandBatch, r.grant.LandMembers = false, nil
	r.grant.Branch, r.grant.BaseRef, r.grant.ApprovedHead, r.grant.LandMessage = a.Branch, a.BaseRef, a.ApprovedHead, a.Message
	r.grant.LandReady, r.grant.LandRecheck = ".brigada/land-ready.sh", true
	r.cfg.CheckTimeout = 30 * time.Second
	before := remoteBranchCommit(t, remote, "main")
	r.runLandSquashPhase(context.Background())
	if got := remoteBranchCommit(t, remote, "main"); got != before {
		t.Errorf("main moved to %s on a re-check, want %s: a re-check lands nothing", got, before)
	}
	if !slices.ContainsFunc(bodies, func(s string) bool { return strings.Contains(s, `"outcome":"ready"`) }) {
		t.Errorf("completions %q, want the ready outcome", bodies)
	}
}

// TestLandBatchReportsProgress (§7.1): the run reports each park and skip
// as it is reached and, before the push, every survivor's input, check
// and publication intent with its place in the stack.
func TestLandBatchReportsProgress(t *testing.T) {
	remote := newRemote(t)
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	b := batchMember(t, remote, 2, "main", nil, map[string]string{"README.md": "# b's seed\n"})
	c := batchMember(t, remote, 3, b.Branch, []int64{2}, map[string]string{"c.txt": "c\n"})
	d := batchMember(t, remote, 4, "main", nil, map[string]string{"d.txt": "d\n"})
	pushToMain(t, remote, "README.md", "# another engineer's seed\n")
	members := []engine.LandMember{a, b, c, d}
	r, dir := batchRun(t, remote, engine.GateAuto, "", members...)
	var bodies []string
	r.client = capturingClient(&bodies)
	if _, errMsg, _ := batchLoop(r, dir); errMsg != "" {
		t.Fatalf("the batch stopped: %s", errMsg)
	}
	var progress []string
	for _, b := range bodies {
		if strings.Contains(b, `"members":[`) && !strings.Contains(b, `"result"`) {
			progress = append(progress, b)
		}
	}
	joined := strings.Join(progress, "\n")
	for _, want := range []string{`"task_id":2,"outcome":"parked"`, `"task_id":3,"outcome":"dropped","blocked_by":2`,
		`"intent":"target","position":1`, `"intent":"target","position":2`} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress reports %s lack %s", joined, want)
		}
	}
}
