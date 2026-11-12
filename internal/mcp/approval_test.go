package mcp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/approval"
)

// The coordinator's plan_approve and plan_reject only request (§12.2):
// they create the pending approval and return its challenge, and the user
// confirms out of band with a key of their own — `brigada plan approve
// --request N --signature FILE`, which these tests stand in for with the
// API client the command uses. The signatures are real: made by
// `ssh-keygen -Y sign -f <key> -n brigada-approval <file holding the
// challenge's message>`. A host without ssh-keygen fails these tests.

type toolSigner struct{ private, public string }

func newToolSigner(t *testing.T) toolSigner {
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
	return toolSigner{private, string(pub)}
}

func (s toolSigner) enroll(t *testing.T, e *env) {
	t.Helper()
	if err := approval.NewStore(e.keys).Add(approval.Key{Label: "me", PublicKey: s.public}); err != nil {
		t.Fatal(err)
	}
}

func (s toolSigner) sign(t *testing.T, message string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "challenge")
	if err := os.WriteFile(file, []byte(message), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ssh-keygen", "-Y", "sign", "-f", s.private, "-n", approval.Namespace, file).CombinedOutput(); err != nil {
		t.Fatalf("sign: %v: %s", err, out)
	}
	sig, err := os.ReadFile(file + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	return string(sig)
}

var requestNumberRE = regexp.MustCompile(`\(request #(\d+), challenge ([0-9a-f]{32})`)

// pendingOf reads the request id, the challenge and the message to sign
// out of a tool's text, the way a user following it would.
func pendingOf(t *testing.T, text string) (id int64, challenge, message string) {
	t.Helper()
	m := requestNumberRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("text %q names no pending request", text)
	}
	id, _ = strconv.ParseInt(m[1], 10, 64)
	var msg strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "    ") {
			msg.WriteString(strings.TrimPrefix(line, "    ") + "\n")
		}
	}
	return id, m[2], msg.String()
}

// confirm is the user's side: the signature made out of band, presented
// the way `brigada plan approve|reject --request N --signature FILE` does.
func confirm(t *testing.T, e *env, verb string, plan, version, request int64, signature string) error {
	t.Helper()
	c := e.client.WithApproval(api.ApprovalProof{RequestID: request, Signature: signature})
	var err error
	if verb == "approve" {
		_, err = c.ApprovePlanVersion(t.Context(), coordRepo, plan, version, false)
	} else {
		_, err = c.RejectPlanVersion(t.Context(), coordRepo, plan, version)
	}
	return err
}

// requestAndConfirm makes a gated tool call and confirms it out of band:
// the tool requests (a result, not an error, and the plan unmoved), the
// challenge in its text is signed, and the user's confirmation proceeds.
// It returns "plan N approved|rejected" for the assertions of callers.
func requestAndConfirm(t *testing.T, e *env, s *session, signer toolSigner, id int, name string, args map[string]any) (string, bool) {
	t.Helper()
	verb := strings.TrimPrefix(name, "plan_")
	text, isErr := toolText(t, s.call(id, "tools/call", map[string]any{"name": name, "arguments": args}))
	if isErr || !strings.Contains(text, "this call only requested it") || !strings.Contains(text, "signature wanted out of band") {
		t.Fatalf("%s = %q (error %v), want a request awaiting the user", name, text, isErr)
	}
	plan := int64(args["plan"].(int64))
	if st := planStateOf(t, e, plan); st != "pending_approval" {
		t.Fatalf("%s moved plan %d to %s", name, plan, st)
	}
	request, _, message := pendingOf(t, text)
	if err := confirm(t, e, verb, plan, int64(args["version"].(int)), request, signer.sign(t, message)); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("plan %d %s", plan, map[string]string{"approve": "approved", "reject": "rejected"}[verb]), false
}

// TestCoordinatorPlanDecisionsAreRequestsTheUserConfirmsOutOfBand: a
// token alone — the coordinator's — approves and rejects nothing; the tool
// returns the challenge and the command the user runs; a wrong signature
// and a reused one are refused with their reasons; the real signature of
// the user proceeds.
func TestCoordinatorPlanDecisionsAreRequestsTheUserConfirmsOutOfBand(t *testing.T) {
	e := newEnv(t)
	s := startCoordinator(t, e)
	approveID := waitingPlan(t, e, "a")
	rejectID := waitingPlan(t, e, "b")
	ask := func(id int, name string, plan int64, extra map[string]any) (string, bool) {
		args := map[string]any{"repo_url": coordRepo, "plan": plan, "version": 1}
		for k, v := range extra {
			args[k] = v
		}
		return toolText(t, s.call(id, "tools/call", map[string]any{"name": name, "arguments": args}))
	}

	// The fail-closed path: no approval key is enrolled, so the call says
	// so, names the command to enroll one, and approves nothing.
	text, isErr := ask(1, "plan_approve", approveID, nil)
	if !isErr || !strings.Contains(text, approval.ErrNoKey.Error()) || !strings.Contains(text, "brigada approver add") || !strings.Contains(text, "is not approved") {
		t.Fatalf("plan_approve with no key = %q (error %v)", text, isErr)
	}
	if got := planStateOf(t, e, approveID); got != "pending_approval" {
		t.Fatalf("plan %d is %s after a refusal", approveID, got)
	}

	signer := newToolSigner(t)
	signer.enroll(t, e)

	// A request: a result with the challenge, the out-of-band wording and
	// the exact command the user runs; the plan stays unmoved.
	text, isErr = ask(2, "plan_reject", rejectID, nil)
	request, challenge, message := pendingOf(t, text)
	if isErr || !strings.Contains(text, "NOT rejected") || !strings.Contains(text, "out of band") ||
		!strings.Contains(text, fmt.Sprintf("brigada plan reject --request %d --signature <file>.sig %d", request, rejectID)) ||
		!strings.Contains(message, challenge) || !strings.Contains(message, "act: plan reject:") {
		t.Fatalf("plan_reject request = %q (error %v)", text, isErr)
	}
	if got := planStateOf(t, e, rejectID); got != "pending_approval" {
		t.Fatalf("plan %d is %s without a signature", rejectID, got)
	}
	// Asking again finds the same pending request.
	again, _ := ask(3, "plan_reject", rejectID, nil)
	if id2, ch2, _ := pendingOf(t, again); id2 != request || ch2 != challenge {
		t.Errorf("a second request = #%d %s, want #%d %s", id2, ch2, request, challenge)
	}

	// A wrong signature — here a token — is refused; the request waits.
	if err := confirm(t, e, "reject", rejectID, 1, request, "client-secret-9"); err == nil || !strings.Contains(err.Error(), approval.ErrBadSignature.Error()) {
		t.Fatalf("a token as the signature: %v, want a wrong signature", err)
	}
	// The signature of another plan's challenge is no better.
	otherText, _ := ask(4, "plan_approve", approveID, nil)
	_, _, otherMessage := pendingOf(t, otherText)
	if err := confirm(t, e, "reject", rejectID, 1, request, signer.sign(t, otherMessage)); err == nil || !strings.Contains(err.Error(), approval.ErrBadSignature.Error()) {
		t.Fatalf("another act's signature: %v, want a wrong signature", err)
	}
	if got := planStateOf(t, e, rejectID); got != "pending_approval" {
		t.Fatalf("plan %d is %s after refused signatures", rejectID, got)
	}

	// The user's real signature proceeds, once.
	sig := signer.sign(t, message)
	if err := confirm(t, e, "reject", rejectID, 1, request, sig); err != nil {
		t.Fatalf("the user's signature: %v", err)
	}
	if got := planStateOf(t, e, rejectID); got != "cancelled" {
		t.Errorf("plan %d is %s, want cancelled", rejectID, got)
	}
	if err := confirm(t, e, "reject", rejectID, 1, request, sig); err == nil {
		t.Error("a reused signature passed")
	} else if !strings.Contains(err.Error(), approval.ErrReused.Error()) && !strings.Contains(err.Error(), "not awaiting") {
		t.Errorf("reused signature: %v, want it refused", err)
	}
	if got := planStateOf(t, e, approveID); got != "pending_approval" {
		t.Errorf("the other plan is %s: a signature released an act it did not name", got)
	}
}
