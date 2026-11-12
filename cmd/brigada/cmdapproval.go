package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Blackfire-AI/brigada/internal/api"
)

// connectGate is how the gated commands reach the engine; tests swap it
// to serve them over an in-process API, as connectPause does.
var connectGate = connect

// maxSignatureBytes bounds a signature file: an ssh-keygen signature is
// a few hundred bytes of armor.
const maxSignatureBytes = 64 << 10

// approvalFlags are the two flags the user's signed approvals take (§12.2)
// on `plan approve|reject`, `land approve|reject` and `push approve`: the
// pending request the call answers and the file holding the signature of
// its challenge. A call without them asks, and the engine answers with the
// challenge to sign out of band.
type approvalFlags struct {
	request   int64
	signature string
}

func addApprovalFlags(fs *flag.FlagSet) *approvalFlags {
	a := &approvalFlags{}
	fs.Int64Var(&a.request, "request", 0, "the pending approval request this call answers, as the refusal printed it")
	fs.StringVar(&a.signature, "signature", "", "the file holding the signature of that request's challenge, as ssh-keygen -Y sign wrote it (<file>.sig)")
	return a
}

// proof reads the flags into the proof a gated call carries. Both flags
// go together; neither asks. A bad pair is a usage error, code 2.
func (a *approvalFlags) proof(stderr io.Writer) (api.ApprovalProof, int) {
	switch {
	case a.request == 0 && a.signature == "":
		return api.ApprovalProof{}, 0
	case a.request <= 0 || a.signature == "":
		return api.ApprovalProof{}, usagef(stderr, "--request and --signature go together: the request the signature answers and the file holding it")
	}
	f, err := os.Open(a.signature)
	if err != nil {
		return api.ApprovalProof{}, fail(stderr, fmt.Errorf("read the signature: %w", err))
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSignatureBytes+1))
	if err != nil {
		return api.ApprovalProof{}, fail(stderr, fmt.Errorf("read the signature: %w", err))
	}
	if len(data) > maxSignatureBytes {
		return api.ApprovalProof{}, fail(stderr, fmt.Errorf("%s is over %d bytes, so it is not a signature", a.signature, maxSignatureBytes))
	}
	return api.ApprovalProof{RequestID: a.request, Signature: string(data)}, 0
}
