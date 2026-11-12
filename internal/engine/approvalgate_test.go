package engine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/approval"
)

// The acceptance tests of the approval authority behind the gates run
// against real signatures made and verified by ssh-keygen. The check host
// has it, so a missing binary fails these tests: a skipped signature test
// would let the criteria pass unverified.

type gateSigner struct {
	private string
	public  string
}

func newGateSigner(t *testing.T) gateSigner {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Fatalf("ssh-keygen is required on the check host for the approval tests: %v", err)
	}
	private := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", private).CombinedOutput(); err != nil {
		t.Fatalf("generate a key: %v: %s", err, out)
	}
	pub, err := os.ReadFile(private + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return gateSigner{private: private, public: string(pub)}
}

// sign signs the pending request's message under the approval namespace.
func (s gateSigner) sign(t *testing.T, p *PendingApproval) []byte {
	t.Helper()
	file := filepath.Join(t.TempDir(), "msg")
	if err := os.WriteFile(file, []byte(p.Message), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ssh-keygen", "-Y", "sign", "-f", s.private, "-n", p.Namespace, file).CombinedOutput(); err != nil {
		t.Fatalf("sign: %v: %s", err, out)
	}
	sig, err := os.ReadFile(file + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// gateEnv is an env whose engine reads approval keys from a file of its own.
func gateEnv(t *testing.T) (*env, string) {
	t.Helper()
	keys := filepath.Join(t.TempDir(), "approval_keys")
	e := newEnvCfg(t, func(c *Config) { c.ApprovalKeys = keys })
	return e, keys
}

func enrollGateKey(t *testing.T, keys string, s gateSigner) {
	t.Helper()
	if err := approval.NewStore(keys).Add(approval.Key{Label: "me", PublicKey: s.public}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) planAwaiting(spec string) int64 {
	e.t.Helper()
	e.set(map[string]string{SettingGatePlan: GateAsk})
	res := e.createPlan([]PlanTask{{Spec: spec}})
	e.assertAwaits(res, "gate_plan")
	return res.PlanID
}

func (e *env) ask(act GatedActRequest) *PendingApproval {
	e.t.Helper()
	res, err := e.eng.GatedAct(e.ctx, act, 0, nil)
	if err != nil || res.Pending == nil {
		e.t.Fatalf("ask %+v = %+v, %v; want a pending request", act, res, err)
	}
	return res.Pending
}

func (e *env) planState(plan int64) string {
	return e.str("SELECT state FROM plans WHERE id = ?", plan)
}

func (e *env) requestState(id int64) string {
	return e.str("SELECT state FROM approval_requests WHERE id = ?", id)
}

func TestGatedAskCreatesAPendingRequestAndRunsNothing(t *testing.T) {
	e, keys := gateEnv(t)
	plan := e.planAwaiting("one")
	p := e.ask(GatedActRequest{Action: GatedPlanApprove, Plan: plan})

	if e.planState(plan) != planAwaitingApproval {
		t.Fatalf("plan state = %s after only asking, want it still awaiting", e.planState(plan))
	}
	if e.requestState(p.RequestID) != "waiting" || p.Challenge == "" || p.Message == "" || p.Namespace != approval.Namespace {
		t.Errorf("pending = %+v, state %s", p, e.requestState(p.RequestID))
	}
	if !p.NoKey {
		t.Error("NoKey = false with no key enrolled")
	}
	if !strings.Contains(p.Message, "plan approve:project=") || !strings.Contains(p.Message, p.Challenge) {
		t.Errorf("message does not bind the act and the challenge: %q", p.Message)
	}
	// Asking again for the same act finds the same request.
	if again := e.ask(GatedActRequest{Action: GatedPlanApprove, Plan: plan}); again.RequestID != p.RequestID || again.Challenge != p.Challenge {
		t.Errorf("second ask = %+v, want request %d again", again, p.RequestID)
	}
	if got := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evApprovalWanted); got != 1 {
		t.Errorf("approval_signature_wanted events = %d, want 1", got)
	}
	// An act named differently is another request.
	if other := e.ask(GatedActRequest{Action: GatedPlanReject, Plan: plan}); other.RequestID == p.RequestID {
		t.Error("rejecting the plan shares the approving request")
	}

	// The needs-you queue names the challenge and the act, out of band.
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range st.NeedsYou {
		if it.Kind == "approval" && it.ID == p.RequestID {
			found = true
			for _, want := range []string{"signature wanted out of band", "approve plan #", p.Challenge, "request #"} {
				if !strings.Contains(it.Reason, want) {
					t.Errorf("needs-you reason %q lacks %q", it.Reason, want)
				}
			}
			if it.Reason != p.Wanted {
				t.Errorf("needs-you reason %q differs from the call's wording %q", it.Reason, p.Wanted)
			}
		}
	}
	if !found {
		t.Errorf("needs-you = %+v, want the pending approval", st.NeedsYou)
	}
	_ = keys
}

func TestGatedVerifiedSignatureReleasesExactlyThatAct(t *testing.T) {
	e, keys := gateEnv(t)
	signer := newGateSigner(t)
	enrollGateKey(t, keys, signer)
	one := e.planAwaiting("one")
	two := e.createPlan([]PlanTask{{Spec: "two"}}).PlanID

	p1 := e.ask(GatedActRequest{Action: GatedPlanApprove, Plan: one})
	p2 := e.ask(GatedActRequest{Action: GatedPlanApprove, Plan: two})
	if p1.NoKey {
		t.Error("NoKey = true with a key enrolled")
	}
	res, err := e.eng.GatedAct(e.ctx, GatedActRequest{Action: GatedPlanApprove, Plan: one}, p1.RequestID, signer.sign(t, p1))
	if err != nil || res.Plan == nil || res.Plan.State != "active" {
		t.Fatalf("approve plan one = %+v, %v", res, err)
	}
	if e.planState(one) != "active" || e.planState(two) != planAwaitingApproval {
		t.Errorf("plan states = %s, %s; want only the signed plan released", e.planState(one), e.planState(two))
	}
	if e.requestState(p1.RequestID) != "signed" || e.requestState(p2.RequestID) != "waiting" {
		t.Errorf("request states = %s, %s", e.requestState(p1.RequestID), e.requestState(p2.RequestID))
	}
}

func TestGatedRefusalsEachLeaveAPendingRequestToSign(t *testing.T) {
	e, keys := gateEnv(t)
	signer := newGateSigner(t)
	plan := e.planAwaiting("one")
	other := e.createPlan([]PlanTask{{Spec: "two"}}).PlanID
	act := GatedActRequest{Action: GatedPlanApprove, Plan: plan}
	p := e.ask(act)
	otherPending := e.ask(GatedActRequest{Action: GatedPlanApprove, Plan: other})

	refuse := func(name string, want error, act GatedActRequest, id int64, sig []byte) {
		t.Helper()
		res, err := e.eng.GatedAct(e.ctx, act, id, sig)
		if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", name, err, want)
		}
		if res.Plan != nil || e.planState(plan) != planAwaitingApproval {
			t.Errorf("%s: the plan moved to %s", name, e.planState(plan))
		}
		if e.requestState(p.RequestID) != "waiting" {
			t.Errorf("%s: the request is %s, want it still waiting", name, e.requestState(p.RequestID))
		}
	}

	// No key is enrolled yet: even a good signature has nothing to verify against.
	refuse("no enrolled key", approval.ErrNoKey, act, p.RequestID, signer.sign(t, p))

	enrollGateKey(t, keys, signer)
	refuse("no signature", approval.ErrNoSignature, act, p.RequestID, nil)
	stranger := newGateSigner(t)
	refuse("wrong signature", approval.ErrBadSignature, act, p.RequestID, stranger.sign(t, p))
	refuse("signature of another act's challenge", approval.ErrBadSignature, act, p.RequestID, signer.sign(t, otherPending))
	refuse("an act that is not the one signed", approval.ErrWrongAct,
		GatedActRequest{Action: GatedPlanApprove, Plan: other}, p.RequestID, signer.sign(t, p))
	refuse("another action on the same plan", approval.ErrWrongAct,
		GatedActRequest{Action: GatedPlanReject, Plan: plan}, p.RequestID, signer.sign(t, p))
	refuse("another plan version", approval.ErrWrongAct,
		GatedActRequest{Action: GatedPlanApprove, Plan: plan, Version: 7}, p.RequestID, signer.sign(t, p))
	refuse("stop asking, which was not signed for", approval.ErrWrongAct,
		GatedActRequest{Action: GatedPlanApprove, Plan: plan, StopAsking: true}, p.RequestID, signer.sign(t, p))
	if _, err := e.eng.GatedAct(e.ctx, act, 0, []byte("a signature")); !errors.Is(err, approval.ErrNoRequest) {
		t.Errorf("signature naming no request: err = %v, want ErrNoRequest", err)
	}
	if _, err := e.eng.GatedAct(e.ctx, act, p.RequestID+99, []byte("a signature")); !errors.Is(err, approval.ErrNoRequest) {
		t.Errorf("unknown request: err = %v, want ErrNoRequest", err)
	}

	// After all that, the right signature still releases it.
	if res, err := e.eng.GatedAct(e.ctx, act, p.RequestID, signer.sign(t, p)); err != nil || res.Plan == nil {
		t.Fatalf("the right signature: %+v, %v", res, err)
	}
}

func TestGatedSpentChallengeIsRefusedASecondTimeEvenAfterARestart(t *testing.T) {
	e, keys := gateEnv(t)
	signer := newGateSigner(t)
	enrollGateKey(t, keys, signer)
	plan := e.planAwaiting("one")
	act := GatedActRequest{Action: GatedPlanApprove, Plan: plan}
	p := e.ask(act)
	sig := signer.sign(t, p)
	if _, err := e.eng.GatedAct(e.ctx, act, p.RequestID, sig); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.GatedAct(e.ctx, act, p.RequestID, sig); !errors.Is(err, approval.ErrReused) {
		t.Fatalf("second use: err = %v, want ErrReused", err)
	}

	// A new engine over the same store, as after a restart.
	restarted := New(e.st, Config{Now: e.clock.Now, ApprovalKeys: keys})
	if _, err := restarted.GatedAct(e.ctx, act, p.RequestID, sig); !errors.Is(err, approval.ErrReused) {
		t.Fatalf("after a restart: err = %v, want ErrReused", err)
	}
}

func TestGatedExpiredRequestReadsAsExpired(t *testing.T) {
	e, keys := gateEnv(t)
	signer := newGateSigner(t)
	enrollGateKey(t, keys, signer)
	plan := e.planAwaiting("one")
	act := GatedActRequest{Action: GatedPlanApprove, Plan: plan}
	p := e.ask(act)
	sig := signer.sign(t, p)

	e.clock.Advance(approval.MaxTTL + time.Minute)
	if _, err := e.eng.GatedAct(e.ctx, act, p.RequestID, sig); !errors.Is(err, approval.ErrExpired) {
		t.Fatalf("late signature: err = %v, want ErrExpired", err)
	}
	if got := e.requestState(p.RequestID); got != "expired" {
		t.Errorf("request state = %s, want expired", got)
	}
	if e.planState(plan) != planAwaitingApproval {
		t.Errorf("plan state = %s, want it still awaiting", e.planState(plan))
	}
	st, err := e.eng.Status(e.ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range st.NeedsYou {
		if it.Kind == "approval" {
			t.Errorf("the queue still lists an expired request: %+v", it)
		}
	}
	// The user asks again and gets a fresh challenge.
	if again := e.ask(act); again.RequestID == p.RequestID || again.Challenge == p.Challenge {
		t.Errorf("ask after expiry = %+v, want a new request", again)
	}
}

func TestGatedTokenAloneApprovesNothing(t *testing.T) {
	e, keys := gateEnv(t)
	plan := e.planAwaiting("one")
	act := GatedActRequest{Action: GatedPlanApprove, Plan: plan}
	p := e.ask(act)
	// The lines of <state>/tokens: a client token and the admin entry.
	for _, token := range []string{"cli client 6f0e1c2d3b4a59687766554433221100", "admin admin 00112233445566778899aabbccddeeff"} {
		if _, err := e.eng.GatedAct(e.ctx, act, p.RequestID, []byte(token)); !errors.Is(err, approval.ErrNoKey) {
			t.Errorf("token %q with no key enrolled: err = %v, want ErrNoKey", strings.Fields(token)[0], err)
		}
	}
	signer := newGateSigner(t)
	enrollGateKey(t, keys, signer)
	for _, token := range []string{"cli client 6f0e1c2d3b4a59687766554433221100", "admin admin 00112233445566778899aabbccddeeff"} {
		if _, err := e.eng.GatedAct(e.ctx, act, p.RequestID, []byte(token)); !errors.Is(err, approval.ErrBadSignature) {
			t.Errorf("token %q with a key enrolled: err = %v, want ErrBadSignature", strings.Fields(token)[0], err)
		}
	}
	if e.planState(plan) != planAwaitingApproval || e.requestState(p.RequestID) != "waiting" {
		t.Errorf("plan %s, request %s: a token moved something", e.planState(plan), e.requestState(p.RequestID))
	}
}

func TestGatedHeadlessWaitPromptsOnNoTerminal(t *testing.T) {
	e, _ := gateEnv(t)
	plan := e.planAwaiting("one")

	// Standard input is a pipe nobody writes to: a call that read it would block.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	saved := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = saved }()

	done := make(chan *PendingApproval, 1)
	go func() {
		res, err := e.eng.GatedAct(e.ctx, GatedActRequest{Action: GatedPlanApprove, Plan: plan}, 0, nil)
		if err != nil {
			t.Error(err)
		}
		done <- res.Pending
	}()
	select {
	case p := <-done:
		if p == nil || p.RequestID == 0 {
			t.Fatalf("pending = %+v", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call blocked instead of leaving a pending request to be signed out of band")
	}

	// And nothing in the authority opens a terminal.
	for _, dir := range []string{".", "../approval"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"/dev/tty"`) { // a path literal; the docs only name it
				t.Errorf("%s opens /dev/tty", f)
			}
		}
	}
}

func TestGatedLandActsNeedTheirOwnSignature(t *testing.T) {
	e, keys := gateEnv(t)
	signer := newGateSigner(t)
	enrollGateKey(t, keys, signer)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)

	reject := GatedActRequest{Action: GatedLandReject, Task: e.task}
	approve := GatedActRequest{Action: GatedLandApprove, Task: e.task}
	pReject := e.ask(reject)
	pApprove := e.ask(approve)
	if pReject.RequestID == pApprove.RequestID {
		t.Fatal("approving and rejecting a landing share a request")
	}
	// The rejecting signature cannot approve.
	if _, err := e.eng.GatedAct(e.ctx, approve, pReject.RequestID, signer.sign(t, pReject)); !errors.Is(err, approval.ErrWrongAct) {
		t.Fatalf("reject's request used to approve: err = %v, want ErrWrongAct", err)
	}
	if n := e.landJobs(); n != 0 {
		t.Fatalf("land jobs = %d before any approval", n)
	}
	res, err := e.eng.GatedAct(e.ctx, approve, pApprove.RequestID, signer.sign(t, pApprove))
	if err != nil || res.Land == nil || !res.Land.Released || !res.Land.Queued {
		t.Fatalf("land approve = %+v, %v", res, err)
	}
	if n := e.landJobs(); n != 1 {
		t.Errorf("land jobs = %d, want 1", n)
	}
}

func TestGatedLandRejectRunsOnlyWithItsSignature(t *testing.T) {
	e, keys := gateEnv(t)
	signer := newGateSigner(t)
	enrollGateKey(t, keys, signer)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.landGateRun(author, reviewer, GateAsk)
	act := GatedActRequest{Action: GatedLandReject, Task: e.task}
	p := e.ask(act)
	if _, err := e.eng.GatedAct(e.ctx, act, p.RequestID, nil); !errors.Is(err, approval.ErrNoSignature) {
		t.Fatalf("no signature: err = %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'rejected'", e.task); n != 0 {
		t.Fatalf("rejected holds = %d before the signature", n)
	}
	res, err := e.eng.GatedAct(e.ctx, act, p.RequestID, signer.sign(t, p))
	if err != nil || res.Land == nil {
		t.Fatalf("land reject = %+v, %v", res, err)
	}
	if n := e.int("SELECT COUNT(*) FROM land_holds WHERE task_id = ? AND state = 'rejected'", e.task); n != 1 {
		t.Errorf("rejected holds = %d, want 1", n)
	}
}

func TestGatedPushApproveNeedsASignature(t *testing.T) {
	e, keys := gateEnv(t)
	signer := newGateSigner(t)
	enrollGateKey(t, keys, signer)
	act := GatedActRequest{Action: GatedPushApprove, RepoURL: "https://example.invalid/brigada.git"}
	p := e.ask(act)
	if !strings.Contains(p.Act, "push of project #") {
		t.Errorf("act words = %q", p.Act)
	}
	if res, err := e.eng.GatedAct(e.ctx, act, p.RequestID, []byte("not a signature")); !errors.Is(err, approval.ErrBadSignature) || res.Push != nil {
		t.Fatalf("bad signature: %+v, %v", res, err)
	}
	res, err := e.eng.GatedAct(e.ctx, act, p.RequestID, signer.sign(t, p))
	if err != nil || res.Push == nil {
		t.Fatalf("push approve = %+v, %v", res, err)
	}
	if _, err := e.eng.GatedAct(e.ctx, act, 0, nil); err != nil {
		t.Errorf("asking for the next push: %v", err)
	}
	if _, err := e.eng.GatedAct(e.ctx, GatedActRequest{Action: GatedPushApprove}, 0, nil); err == nil {
		t.Error("a push without a repo URL was asked for")
	}
}
