package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/approval"
)

// The handler tests of the gated acts (§12.2): a token alone is refused
// and leaves a pending request, a verified signature passes. They sign
// with a real ssh-keygen, which the check host has, and fail — not skip —
// when it is missing, so the criteria cannot pass unverified. The key is
// made with `ssh-keygen -t ed25519`, the signature with
// `ssh-keygen -Y sign -f <key> -n brigada-approval <file holding the
// challenge's message>`.

type apiSigner struct {
	private string
	public  string
}

func newAPISigner(t *testing.T) apiSigner {
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
	return apiSigner{private: private, public: string(pub)}
}

// sign signs the pending request's message and returns the armored signature.
func (s apiSigner) sign(t *testing.T, p ApprovalPending) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "challenge")
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
	return string(sig)
}

func (e *env) enroll(t *testing.T) apiSigner {
	t.Helper()
	s := newAPISigner(t)
	if err := approval.NewStore(e.keys).Add(approval.Key{Label: "me", PublicKey: s.public}); err != nil {
		t.Fatal(err)
	}
	return s
}

// body renders a request body with the proof beside the call's own fields.
func approvalBody(t *testing.T, fields map[string]any, request int64, signature string) []byte {
	t.Helper()
	if fields == nil {
		fields = map[string]any{}
	}
	if request != 0 {
		fields["request_id"] = request
		fields["signature"] = signature
	}
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// refusal decodes an approval_required answer.
func (e *env) refusal(t *testing.T, code int, data []byte) ApprovalPending {
	t.Helper()
	if code != http.StatusForbidden {
		t.Fatalf("status = %d %s, want 403 approval_required", code, data)
	}
	var body struct {
		Error    string           `json:"error"`
		Code     string           `json:"code"`
		Approval *ApprovalPending `json:"approval"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Code != codeApprovalRequired || body.Approval == nil {
		t.Fatalf("answer = %s, want an approval_required refusal naming the pending request", data)
	}
	if !strings.Contains(body.Error, body.Approval.Wanted) || !strings.Contains(body.Error, body.Approval.Reason) {
		t.Errorf("error %q does not carry the wanted line %q and the reason %q", body.Error, body.Approval.Wanted, body.Approval.Reason)
	}
	return *body.Approval
}

// gatedAct is one gated endpoint, with its seed and its check that the act ran.
type gatedAct struct {
	name   string
	path   func(e *env) string
	fields map[string]any
	seed   func(e *env)
	ran    func(e *env) bool
}

func (e *env) awaitingPlan() int64 {
	e.t.Helper()
	e.exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at) VALUES (?, 'to approve', 'pending_approval', ?, ?)`, e.project1, seedTS, seedTS)
	id := int64(e.count(e.t, "SELECT MAX(id) FROM plans"))
	e.exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
		VALUES (?, 'its task', 'spec', 'go test ./...', 'implement', 'pending', 'main', '{}', ?, ?)`, id, seedTS, seedTS)
	return id
}

func gatedActs() []gatedAct {
	var plan int64
	return []gatedAct{
		{
			name: "plan approve",
			seed: func(e *env) { plan = e.awaitingPlan() },
			path: func(e *env) string { return fmt.Sprintf("/v1/plans/%d/approve", plan) },
			ran:  func(e *env) bool { return e.str("SELECT state FROM plans WHERE id = ?", plan) == "active" },
		},
		{
			name: "plan reject",
			seed: func(e *env) { plan = e.awaitingPlan() },
			path: func(e *env) string { return fmt.Sprintf("/v1/plans/%d/reject", plan) },
			ran:  func(e *env) bool { return e.str("SELECT state FROM plans WHERE id = ?", plan) == "cancelled" },
		},
		{
			name: "land approve",
			seed: func(e *env) { e.seedLandHold() },
			path: func(e *env) string { return fmt.Sprintf("/v1/tasks/%d/land/approve", e.task) },
			ran: func(e *env) bool {
				return e.count(e.t, "SELECT COUNT(*) FROM land_holds WHERE state = 'held'") == 0 &&
					e.count(e.t, "SELECT COUNT(*) FROM land_holds WHERE state = 'rejected'") == 0
			},
		},
		{
			name: "land reject",
			seed: func(e *env) { e.seedLandHold() },
			path: func(e *env) string { return fmt.Sprintf("/v1/tasks/%d/land/reject", e.task) },
			ran: func(e *env) bool {
				return e.str("SELECT state FROM land_holds WHERE task_id = ?", e.task) == "rejected"
			},
		},
		{
			name: "push approve",
			seed: func(e *env) {
				e.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
					VALUES (?, ?, 'main', 'abc123', 'waiting', ?)`, e.task, e.project1, seedTS)
			},
			path:   func(e *env) string { return "/v1/push/approve" },
			fields: map[string]any{"repo_url": "https://example.invalid/one.git"},
			ran: func(e *env) bool {
				return e.count(e.t, fmt.Sprintf("SELECT COUNT(*) FROM jobs WHERE task_id = %d AND kind = 'land' AND status = 'queued'", e.task)) == 1
			},
		},
	}
}

// signedCall makes a gated call the way a user does: it asks, the
// challenge is signed with the enrolled key, and the call is presented
// again with the signature.
func (e *env) signedCall(t *testing.T, signer apiSigner, path, who string, fields map[string]any) (int, []byte) {
	t.Helper()
	code, _, data := e.do(http.MethodPost, path, who, "", approvalBody(t, clone(fields), 0, ""))
	p := e.refusal(t, code, data)
	code, _, data = e.do(http.MethodPost, path, who, "", approvalBody(t, clone(fields), p.RequestID, signer.sign(t, p)))
	return code, data
}

func clone(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestGatedActsRefuseATokenAloneAndPassASignedCall: for every gated act, a
// client token and the admin token alone are refused with a pending
// request to sign, the act untouched; then a verified signature for that
// request passes and the act runs.
func TestGatedActsRefuseATokenAloneAndPassASignedCall(t *testing.T) {
	for _, act := range gatedActs() {
		t.Run(act.name, func(t *testing.T) {
			e := newEnv(t)
			act.seed(e)
			path := act.path(e)

			// No key is enrolled: both tokens are refused and say so, and
			// a request waits all the same.
			for _, who := range []string{"alice", "root"} {
				code, _, data := e.do(http.MethodPost, path, who, "", approvalBody(t, clone(act.fields), 0, ""))
				p := e.refusal(t, code, data)
				if p.Reason != approval.ErrNoKey.Error() || !p.NoKey || p.RequestID == 0 || p.Challenge == "" {
					t.Fatalf("%s with no key enrolled = %+v", who, p)
				}
			}
			if act.ran(e) {
				t.Fatal("the act ran on a token alone")
			}

			signer := e.enroll(t)
			code, _, data := e.do(http.MethodPost, path, "alice", "", approvalBody(t, clone(act.fields), 0, ""))
			p := e.refusal(t, code, data)
			if p.Reason != approval.ErrNoSignature.Error() || p.NoKey {
				t.Fatalf("token alone with a key enrolled = %+v, want no signature", p)
			}
			if !strings.Contains(p.Wanted, "signature wanted out of band") {
				t.Errorf("wanted line = %q", p.Wanted)
			}
			// A token passed off as a signature is a wrong signature, with the
			// same request left to sign.
			code, _, data = e.do(http.MethodPost, path, "root", "", approvalBody(t, clone(act.fields), p.RequestID, "root admin root-secret-1"))
			if bad := e.refusal(t, code, data); bad.Reason != approval.ErrBadSignature.Error() || bad.RequestID != p.RequestID {
				t.Fatalf("a token as the signature = %+v, want a wrong signature on request %d", bad, p.RequestID)
			}
			if act.ran(e) {
				t.Fatal("the act ran without a verified signature")
			}

			code, _, data = e.do(http.MethodPost, path, "alice", "", approvalBody(t, clone(act.fields), p.RequestID, signer.sign(t, p)))
			if code != http.StatusOK {
				t.Fatalf("signed call = %d %s, want 200", code, data)
			}
			if !act.ran(e) {
				t.Errorf("the act did not run after a verified signature")
			}
			// The same signature a second time is refused as reused, with a fresh
			// request to sign if the act can still run (it cannot here).
			code, _, data = e.do(http.MethodPost, path, "alice", "", approvalBody(t, clone(act.fields), p.RequestID, signer.sign(t, p)))
			if code == http.StatusOK {
				t.Errorf("a spent signature passed a second time: %s", data)
			}
		})
	}
}

// TestGatedRefusalsNameTheirReasonAndLeaveARequest: each refusal says
// which and leaves a pending request the user can sign out of band.
func TestGatedRefusalsNameTheirReasonAndLeaveARequest(t *testing.T) {
	e := newEnv(t)
	signer := e.enroll(t)
	plan := e.awaitingPlan()
	other := e.awaitingPlan()
	path := fmt.Sprintf("/v1/plans/%d/approve", plan)

	_, _, data := e.do(http.MethodPost, path, "alice", "", nil)
	p := e.refusal(t, http.StatusForbidden, data)
	otherPending := ApprovalPending{}
	{
		code, _, d := e.do(http.MethodPost, fmt.Sprintf("/v1/plans/%d/approve", other), "alice", "", nil)
		otherPending = e.refusal(t, code, d)
	}

	for _, c := range []struct {
		name   string
		path   string
		req    int64
		sig    string
		reason string
	}{
		{"a signature naming no request", path, 0, "", approval.ErrNoSignature.Error()},
		{"a wrong signature", path, p.RequestID, "not a signature", approval.ErrBadSignature.Error()},
		{"the signature of another act's challenge", path, p.RequestID, signer.sign(t, otherPending), approval.ErrBadSignature.Error()},
		{"an act that is not the one signed", fmt.Sprintf("/v1/plans/%d/approve", other), p.RequestID, signer.sign(t, p), approval.ErrWrongAct.Error()},
		{"an unknown request", path, p.RequestID + 99, signer.sign(t, p), approval.ErrNoRequest.Error()},
	} {
		code, _, data := e.do(http.MethodPost, c.path, "alice", "", approvalBody(t, nil, c.req, c.sig))
		got := e.refusal(t, code, data)
		if got.Reason != c.reason {
			t.Errorf("%s: reason = %q, want %q", c.name, got.Reason, c.reason)
		}
		if got.RequestID == 0 || got.Message == "" {
			t.Errorf("%s: no pending request to sign in %+v", c.name, got)
		}
	}
	if e.str("SELECT state FROM plans WHERE id = ?", plan) != "pending_approval" {
		t.Error("a refusal moved the plan")
	}

	// An expired challenge: the request reads as expired and a fresh one is offered.
	sig := signer.sign(t, p)
	e.clock.Advance(approval.MaxTTL + 1)
	code, _, data := e.do(http.MethodPost, path, "alice", "", approvalBody(t, nil, p.RequestID, sig))
	got := e.refusal(t, code, data)
	if got.Reason != approval.ErrExpired.Error() || got.RequestID == p.RequestID {
		t.Errorf("expired = %+v, want ErrExpired and a fresh request", got)
	}
	if st := e.str("SELECT state FROM approval_requests WHERE id = ?", p.RequestID); st != "expired" {
		t.Errorf("the old request is %s, want expired", st)
	}
}

// TestGatedActWaitsOutOfBandAndProceeds is the end-to-end path through
// the client the CLI and the MCP bridge use: the act is refused, the user
// signs the challenge out of band with ssh-keygen -Y sign, the same call is
// presented again with the signature, and the act proceeds. The foreman's
// status and the refusal read one wording.
func TestGatedActWaitsOutOfBandAndProceeds(t *testing.T) {
	e := newEnv(t)
	signer := e.enroll(t)
	plan := e.awaitingPlan()
	client := &Client{http: e.client, token: e.secrets["alice"]}

	_, err := client.ApprovePlanVersion(e.ctx, "https://example.invalid/one.git", plan, 1, false)
	p, ok := ApprovalOf(err)
	if !ok {
		t.Fatalf("first call = %v, want an approval_required refusal", err)
	}

	// The queue shows the same line the refusal printed.
	st, err := e.eng.Status(e.ctx, e.project1)
	if err != nil {
		t.Fatal(err)
	}
	var queued string
	for _, it := range st.NeedsYou {
		if it.Kind == "approval" && it.ID == p.RequestID {
			queued = it.Reason
		}
	}
	if queued != p.Wanted {
		t.Errorf("needs-you reason %q differs from the refusal's wanted line %q", queued, p.Wanted)
	}
	if e.str("SELECT state FROM plans WHERE id = ?", plan) != "pending_approval" {
		t.Fatal("the plan moved before a signature")
	}

	// Out of band: the user signs the exact message with their key.
	sig := signer.sign(t, p)

	signed := client.WithApproval(ApprovalProof{RequestID: p.RequestID, Signature: sig})
	if _, err := signed.ApprovePlanVersion(e.ctx, "https://example.invalid/one.git", plan, 1, false); err != nil {
		t.Fatalf("re-presented call: %v", err)
	}
	if st := e.str("SELECT state FROM plans WHERE id = ?", plan); st != "active" {
		t.Errorf("plan state = %s, want active", st)
	}
	st, err = e.eng.Status(e.ctx, e.project1)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range st.NeedsYou {
		if it.Kind == "approval" {
			t.Errorf("the queue still lists %+v", it)
		}
	}
}
