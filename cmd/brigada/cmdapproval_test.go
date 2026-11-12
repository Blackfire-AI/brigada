package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// The gated commands answer a pending request with --request and
// --signature (§12.2). Without them the act waits; a bad pair is a usage
// error before anything is sent.
func TestGatedCommandsTakeTheProofAsAPair(t *testing.T) {
	sig := filepath.Join(t.TempDir(), "challenge.sig")
	if err := os.WriteFile(sig, []byte("armored signature text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	for _, cmd := range [][]string{
		{"plan", "approve", "7"}, {"plan", "reject", "7"},
		{"land", "approve", "7"}, {"land", "reject", "7"},
		{"push", "approve", "--repo", "https://example.invalid/one.git"},
	} {
		name := strings.Join(cmd[:2], " ")
		// Flags come before the positional argument.
		build := func(flags ...string) []string {
			args := append([]string{}, cmd[:2]...)
			args = append(args, flags...)
			args = append(args, "--state-dir", state)
			return append(args, cmd[2:]...)
		}
		for label, extra := range map[string][]string{
			"request alone":        {"--request", "5"},
			"signature alone":      {"--signature", sig},
			"request not positive": {"--request", "-3", "--signature", sig},
		} {
			var out, errb bytes.Buffer
			if code := run(context.Background(), build(extra...), &out, &errb); code != 2 || !strings.Contains(errb.String(), "--request and --signature go together") {
				t.Errorf("%s with %s: exit %d, stderr %q; want a usage error", name, label, code, errb.String())
			}
		}
		var out, errb bytes.Buffer
		if code := run(context.Background(), build("--request", "5", "--signature", filepath.Join(state, "missing.sig")), &out, &errb); code != 1 || !strings.Contains(errb.String(), "read the signature") {
			t.Errorf("%s with a missing signature file: exit %d, stderr %q", name, code, errb.String())
		}
		var misspelled bytes.Buffer
		if code := run(context.Background(), build("--signatur", sig), &out, &misspelled); code != 2 {
			t.Errorf("%s with --signatur: exit %d, want 2", name, code)
		}
	}

	flags := &approvalFlags{request: 5, signature: sig}
	proof, code := flags.proof(&bytes.Buffer{})
	if code != 0 || proof.RequestID != 5 || proof.Signature != "armored signature text\n" {
		t.Errorf("proof = %+v, code %d", proof, code)
	}
	if proof, code := (&approvalFlags{}).proof(&bytes.Buffer{}); code != 0 || proof != (api.ApprovalProof{}) {
		t.Errorf("no flags = %+v, code %d, want the empty proof, which asks", proof, code)
	}
}

// A refusal or a wait prints the reason, the pending request, the message
// to sign and the commands, the same text the MCP bridge returns.
func TestFailPrintsAnApprovalRefusalWithItsChallenge(t *testing.T) {
	pending := api.ApprovalPending{
		RequestID: 5, Act: "approve plan #7 at version 2", Challenge: "0123456789abcdef0123456789abcdef",
		Message:   "brigada approval v1\nact: plan approve:project=1:plan=7:v=2:stop=0\nnonce: 0123456789abcdef0123456789abcdef\nexpires: 1\n",
		Namespace: "brigada-approval", Reason: "no signature was given",
		Wanted: "signature wanted out of band to approve plan #7 at version 2 (request #5, challenge 0123456789abcdef0123456789abcdef, expires 2026-11-13T10:05:00Z)",
	}
	body, err := json.Marshal(map[string]any{"error": pending.Summary(), "code": "approval_required", "approval": pending})
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	code := fail(&errb, &api.StatusError{Method: "POST", Path: "/v1/plans/7/approve", Status: 403, APICode: "approval_required", Message: pending.Summary(), Body: body})
	out := errb.String()
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	for _, want := range []string{
		"approval required: no signature was given", pending.Wanted, "    act: plan approve:project=1:plan=7:v=2:stop=0",
		"ssh-keygen -Y sign -f <your private key> -n brigada-approval", "--request 5 --signature <file>.sig",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
}
