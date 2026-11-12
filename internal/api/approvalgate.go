package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/approval"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The user's approvals are signed (§12.2). A role (client or admin)
// authorizes the call; the act itself — a plan's yes or no at its
// version, a landing's yes or no, a push's yes — runs only when the call
// also carries the signature of the pending request made for exactly that
// act (engine.GatedAct). A call without one is not an error of the
// caller's making and not a success either: the act waits. The answer is
// the same 403 `approval_required` for a wait and for every refusal, so a
// foreman, the CLI and the MCP bridge read one wording; the error body
// names the pending request to sign out of band.

const codeApprovalRequired = "approval_required"

// ApprovalProof is what a gated call carries to answer a pending request:
// the request's id and the armored ssh-keygen signature of its challenge
// message. Both are empty on a first call, which asks.
type ApprovalProof struct {
	RequestID int64  `json:"request_id,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// ApprovalPending is the structured part of an approval_required refusal:
// the request that waits for a signature and why the call was refused.
type ApprovalPending struct {
	RequestID int64     `json:"request_id"`
	Act       string    `json:"act"`
	Challenge string    `json:"challenge"`
	Message   string    `json:"message"` // the exact text to sign
	Namespace string    `json:"namespace"`
	Expires   time.Time `json:"expires"`
	Wanted    string    `json:"wanted"` // the one line every surface prints
	Reason    string    `json:"reason"` // why the call did not run the act
	NoKey     bool      `json:"no_key,omitempty"`
}

// Summary is the refusal's one line, the error message of the API.
func (a ApprovalPending) Summary() string {
	return fmt.Sprintf("approval required: %s; %s", a.Reason, a.Wanted)
}

// Text is the refusal in full, for a terminal or a tool result: the
// reason, the wanted line, the exact message to sign and the commands.
func (a ApprovalPending) Text() string {
	var b strings.Builder
	b.WriteString(a.Summary() + "\n")
	if a.NoKey {
		b.WriteString("enroll an approval key first: brigada approver add --label <label> --public-key <file>\n")
	}
	b.WriteString("the message to sign, exactly these lines, saved to a file:\n")
	for _, line := range strings.Split(strings.TrimRight(a.Message, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	fmt.Fprintf(&b, "sign it with a key you enrolled: ssh-keygen -Y sign -f <your private key> -n %s <file>\n", a.Namespace)
	fmt.Fprintf(&b, "then confirm it with request %d and the signature in <file>.sig", a.RequestID)
	return b.String()
}

// ApprovalOf reads the pending request out of an approval_required
// refusal; ok is false for any other error.
func ApprovalOf(err error) (ApprovalPending, bool) {
	var se *StatusError
	if !errors.As(err, &se) || se.APICode != codeApprovalRequired {
		return ApprovalPending{}, false
	}
	var body struct {
		Approval *ApprovalPending `json:"approval"`
	}
	if json.Unmarshal(se.Body, &body) != nil || body.Approval == nil {
		return ApprovalPending{}, false
	}
	return *body.Approval, true
}

// approvalReasons maps each refusal of internal/approval to the words
// every surface uses for it.
var approvalReasons = []error{
	approval.ErrNoKey, approval.ErrNoSignature, approval.ErrBadSignature,
	approval.ErrReused, approval.ErrExpired, approval.ErrWrongAct,
	approval.ErrRefused, approval.ErrBadChallenge, approval.ErrUnavailable,
}

// approvalReason words err when it is one of the approval refusals.
func approvalReason(err error, proof ApprovalProof) (string, bool) {
	if errors.Is(err, engine.ErrSignatureWithoutRequest) || (errors.Is(err, approval.ErrNoRequest) && proof.RequestID == 0) {
		return "a signature answers a pending request; name the request it signs", true
	}
	if errors.Is(err, approval.ErrNoRequest) {
		return approval.ErrNoRequest.Error(), true
	}
	for _, reason := range approvalReasons {
		if errors.Is(err, reason) {
			return reason.Error(), true
		}
	}
	return "", false
}

// runGated is the gate in front of one act. It returns the act's result
// when a signature verified and the act ran; written is true when it
// already answered the caller with an approval_required refusal; any
// other error is the act's own, for the handler to map as it always did.
func (s *Server) runGated(w http.ResponseWriter, r *http.Request, act engine.GatedActRequest, proof ApprovalProof) (res engine.GatedResult, written bool, err error) {
	res, err = s.eng.GatedAct(r.Context(), act, proof.RequestID, []byte(proof.Signature))
	var pending *engine.PendingApproval
	var reason string
	switch {
	case err == nil && res.Pending == nil:
		return res, false, nil
	case err == nil:
		// A first call: it asked, and the act waits.
		pending = res.Pending
		reason = approval.ErrNoSignature.Error()
		if pending.NoKey {
			reason = approval.ErrNoKey.Error()
		}
	default:
		why, ok := approvalReason(err, proof)
		if !ok {
			return engine.GatedResult{}, false, err
		}
		reason = why
		// Every refusal leaves a pending request to sign: the waiting one
		// for this act, or a fresh one when the last was spent or expired.
		again, aerr := s.eng.GatedAct(r.Context(), act, 0, nil)
		if aerr != nil || again.Pending == nil {
			if aerr == nil {
				aerr = errors.New("the engine made no pending request")
			}
			return engine.GatedResult{}, false, aerr
		}
		pending = again.Pending
	}
	a := ApprovalPending{
		RequestID: pending.RequestID, Act: pending.Act, Challenge: pending.Challenge,
		Message: pending.Message, Namespace: pending.Namespace, Expires: pending.Expires,
		Wanted: pending.Wanted, Reason: reason, NoKey: pending.NoKey,
	}
	writeJSON(w, http.StatusForbidden, struct {
		Error    string          `json:"error"`
		Code     string          `json:"code"`
		Approval ApprovalPending `json:"approval"`
	}{a.Summary(), codeApprovalRequired, a})
	return engine.GatedResult{}, true, nil
}
