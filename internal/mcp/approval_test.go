package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/approval"
)

// The coordinator's plan_approve and plan_reject carry the user's
// signature like every gated act (§12.2). These tests sign with a real
// ssh-keygen — `ssh-keygen -Y sign -f <key> -n brigada-approval <file
// holding the challenge's message>` — and fail, not skip, when it is
// missing: the check host has it.

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

var requestIDRE = regexp.MustCompile(`request_id (\d+)`)

// pendingOf reads the request id and the message to sign out of a refusal
// text, the way a user following it would.
func pendingOf(t *testing.T, text string) (int64, string) {
	t.Helper()
	m := requestIDRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("refusal %q names no request_id", text)
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	var msg strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "    ") {
			msg.WriteString(strings.TrimPrefix(line, "    ") + "\n")
		}
	}
	return id, msg.String()
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

// signedTool makes a gated tool call the way a user does: it asks, the
// challenge in the refusal is signed out of band, and the call is made
// again with the signature.
func signedTool(t *testing.T, e *env, s *session, signer toolSigner, id int, name string, args map[string]any) (string, bool) {
	t.Helper()
	text, isErr := toolText(t, s.call(id, "tools/call", map[string]any{"name": name, "arguments": args}))
	if !isErr || !strings.Contains(text, "approval required") || !strings.Contains(text, "signature wanted out of band") {
		t.Fatalf("%s asked = %q (error %v), want an approval refusal", name, text, isErr)
	}
	request, message := pendingOf(t, text)
	signed := map[string]any{"request_id": request, "signature": signer.sign(t, message)}
	for k, v := range args {
		signed[k] = v
	}
	return toolText(t, s.call(id+1, "tools/call", map[string]any{"name": name, "arguments": signed}))
}

// TestCoordinatorPlanDecisionsNeedTheUsersSignature: a token alone —
// the coordinator's client token — approves and rejects nothing; the
// refusal names why and the request to sign, and the signed call passes.
func TestCoordinatorPlanDecisionsNeedTheUsersSignature(t *testing.T) {
	e := newEnv(t)
	s := startCoordinator(t, e)
	approveID := waitingPlan(t, e, "a")
	rejectID := waitingPlan(t, e, "b")

	// No key is enrolled: refused, and the text says to enroll one.
	text, isErr := toolText(t, s.call(1, "tools/call", map[string]any{"name": "plan_approve",
		"arguments": map[string]any{"repo_url": coordRepo, "plan": approveID, "version": 1}}))
	if !isErr || !strings.Contains(text, approval.ErrNoKey.Error()) || !strings.Contains(text, "brigada approver add") {
		t.Fatalf("plan_approve with no key = %q (error %v)", text, isErr)
	}
	if got := planStateOf(t, e, approveID); got != "pending_approval" {
		t.Fatalf("plan %d is %s after a refusal", approveID, got)
	}

	signer := newToolSigner(t)
	if err := approval.NewStore(e.keys).Add(approval.Key{Label: "me", PublicKey: signer.public}); err != nil {
		t.Fatal(err)
	}
	text, isErr = toolText(t, s.call(2, "tools/call", map[string]any{"name": "plan_reject",
		"arguments": map[string]any{"repo_url": coordRepo, "plan": rejectID, "version": 1}}))
	if !isErr || !strings.Contains(text, approval.ErrNoSignature.Error()) {
		t.Fatalf("plan_reject with a key enrolled but no signature = %q (error %v)", text, isErr)
	}
	// A token passed off as a signature is a wrong signature.
	request, _ := pendingOf(t, text)
	text, isErr = toolText(t, s.call(3, "tools/call", map[string]any{"name": "plan_reject",
		"arguments": map[string]any{"repo_url": coordRepo, "plan": rejectID, "version": 1, "request_id": request, "signature": "client-secret-9"}}))
	if !isErr || !strings.Contains(text, approval.ErrBadSignature.Error()) {
		t.Fatalf("plan_reject with a token as the signature = %q (error %v)", text, isErr)
	}
	if got := planStateOf(t, e, rejectID); got != "pending_approval" {
		t.Fatalf("plan %d is %s without a signature", rejectID, got)
	}

	if text, isErr := signedTool(t, e, s, signer, 10, "plan_approve", map[string]any{"repo_url": coordRepo, "plan": approveID, "version": 1}); isErr || !strings.Contains(text, "approved") {
		t.Fatalf("signed plan_approve = %q (error %v)", text, isErr)
	}
	if text, isErr := signedTool(t, e, s, signer, 20, "plan_reject", map[string]any{"repo_url": coordRepo, "plan": rejectID, "version": 1}); isErr || !strings.Contains(text, "rejected") {
		t.Fatalf("signed plan_reject = %q (error %v)", text, isErr)
	}
	if a, r := planStateOf(t, e, approveID), planStateOf(t, e, rejectID); a != "active" || r != "cancelled" {
		t.Errorf("plan states = %s, %s; want active, cancelled", a, r)
	}
}
