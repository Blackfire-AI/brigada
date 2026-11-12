// Package approval is the out-of-band authority behind a user's approval of
// a plan, a task, a push or an import (§12.2): a signature made with a key
// the user enrolled, which a process of the same OS user cannot produce
// from a client token alone. A token says who may call; only the private
// key behind an enrolled public key says the user approved this act.
//
// The mechanism is OpenSSH's own `ssh-keygen -Y sign` and `ssh-keygen -Y
// verify` under the brigada namespace. Its consequences:
//
//   - ssh-keygen is a runtime dependency of the approval authority. A
//     missing binary fails closed exactly as an unverified signature does:
//     the act is refused, with ErrUnavailable saying why.
//   - Verification needs an allowed_signers file. The package writes it as
//     a temporary file of mode 0600 inside a private directory and removes
//     it when the call returns.
//   - A test that needs a real signature skips when the binary is absent,
//     and says so.
//
// The authority never prompts on /dev/tty and has no fallback to one: an
// agent with a shell drives a terminal as the same user, so a terminal
// answer proves nothing. With no enrolled key, no signature, a wrong, reused
// or expired one, the act is refused and the error says which.
//
// The enrolled keys are public keys with a label, never a private key, kept
// beside the token file in the engine's state directory (see Store). A
// challenge (see Challenge) names one act and carries a nonce and a short
// expiry; what the user signs is the challenge's message under Namespace.
package approval

import "errors"

// Namespace is the ssh-keygen namespace (-n) every approval signature is
// made and verified under, so a signature made for another purpose with
// the same key (a git commit, a file) never approves a brigada act.
const Namespace = "brigada-approval"

// The reasons an act is refused. Verify wraps one of them with the act's
// detail; test with errors.Is.
var (
	ErrNoKey        = errors.New("no approval key is enrolled")
	ErrNoSignature  = errors.New("no signature was given")
	ErrBadSignature = errors.New("the signature does not verify against an enrolled key")
	ErrReused       = errors.New("the challenge was already answered; its signature cannot be used again")
	ErrExpired      = errors.New("the challenge has expired")
	ErrWrongAct     = errors.New("the challenge was issued for another act")
	ErrBadChallenge = errors.New("the challenge is malformed")
	ErrUnavailable  = errors.New("ssh-keygen is unavailable, so no signature can be verified")
)
