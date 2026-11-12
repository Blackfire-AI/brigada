// Package approval is the user's approval authority (design §12.2): the
// out-of-band proof, beyond any token, that the user — not a process
// running as the user — approved one named act.
//
// A token authenticates its caller and carries no approval authority: any
// process of the same OS user can read the state directory and use one,
// and an agent with a shell can drive a terminal as the user, so neither
// a token nor a prompt on /dev/tty proves anything. This package holds the
// authority instead in an SSH key the user enrolls by its public half
// alone (keys.go): the engine issues a challenge for one act — its kind,
// its identity, a nonce and a short expiry — stores it as a pending
// request (the store's approval_requests table), and accepts only an
// OpenSSH SSHSIG signature over that exact challenge, under the
// brigada-approval namespace, by an enrolled key:
//
//	ssh-keygen -Y sign -n brigada-approval -f <key> <challenge-file>
//
// The property holds as far as the private key is out of the same user's
// reach: a security key (sk-ssh-ed25519@openssh.com, whose signature must
// carry the touch flag) or a key on another device. A key file in ~/.ssh
// that the user's own processes can read does not satisfy it.
//
// Every refusal fails closed and says which check refused: no enrolled
// key, no signature, a malformed or wrong signature, a key not enrolled, a
// reused signature, an expired challenge, or a challenge issued for
// another act. There is no fallback — no TTY prompt, no token.
//
// The gates (gate_plan, gate_land, gate_push, approval_tiers) still decide
// whether an act waits for the user; this package is the authority that
// releases what a gate holds, not a replacement for the gates.
package approval

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind is the kind of act an approval releases.
type Kind string

// The acts that need the user's approval authority (§12.2): a plan, a
// landing held at the land gate, a push held at the push gate, and an
// import of a project's settings.
const (
	KindPlan   Kind = "plan"
	KindLand   Kind = "land"
	KindPush   Kind = "push"
	KindImport Kind = "import"
)

// Act is one named act: its kind and its identity, the plan (with its
// version), the task, the project's push or the import the approval is
// for. A challenge binds both, so an approval of one act approves no other.
type Act struct {
	Kind    Kind
	Subject string
}

func (a Act) String() string { return string(a.Kind) + " " + a.Subject }

func (a Act) check() error {
	switch a.Kind {
	case KindPlan, KindLand, KindPush, KindImport:
	default:
		return fmt.Errorf("unknown approval kind %q", a.Kind)
	}
	if strings.TrimSpace(a.Subject) == "" || strings.ContainsAny(a.Subject, "\r\n") {
		return fmt.Errorf("an approval's subject must be one non-empty line, got %q", a.Subject)
	}
	return nil
}

// DefaultTTL is how long a challenge answers: long enough to sign it,
// short enough that a stale one is useless.
const DefaultTTL = 5 * time.Minute

// The states of a pending request (approval_requests.state).
const (
	StatePending  = "pending"
	StateApproved = "approved"
	StateExpired  = "expired"
)

// timeLayout is the store's timestamp layout, UTC.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// Request is one stored challenge for one act.
type Request struct {
	ID int64
	Act
	Nonce     string
	Challenge string // the exact text to sign, ending in one newline
	CreatedAt time.Time
	ExpiresAt time.Time
	State     string
	// KeyFingerprint is the enrolled key that approved, once approved.
	KeyFingerprint string
}

// The refusals, one per check. Approve wraps one of them with the
// detail, so errors.Is tells the caller which check refused.
var (
	ErrUnknownRequest = errors.New("no such approval request")
	ErrWrongAct       = errors.New("the challenge was issued for another act")
	ErrExpired        = errors.New("the challenge has expired")
	ErrNoKey          = errors.New("no approval key is enrolled")
	ErrNoSignature    = errors.New("no signature was given")
	ErrBadSignature   = errors.New("the signature is not valid for this challenge")
	ErrUnknownKey     = errors.New("the signature is by a key that is not enrolled")
	ErrReused         = errors.New("the signature or challenge has already been used")
)

// challengeText is the text the user signs: every field of the act and
// the challenge, one per line, so a signature cannot be moved to another
// act, another challenge or another time.
func challengeText(act Act, nonce string, expires time.Time) string {
	return fmt.Sprintf("brigada approval challenge\nkind: %s\nsubject: %s\nnonce: %s\nexpires: %s\n",
		act.Kind, act.Subject, nonce, expires.UTC().Format(time.RFC3339))
}

// Issue stores a fresh challenge for act, expiring ttl after now, and
// returns it pending. The nonce is 16 random bytes.
func Issue(ctx context.Context, tx *sql.Tx, act Act, now time.Time, ttl time.Duration) (Request, error) {
	if err := act.check(); err != nil {
		return Request{}, err
	}
	if ttl <= 0 {
		return Request{}, fmt.Errorf("an approval challenge needs a positive lifetime, got %v", ttl)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Request{}, fmt.Errorf("mint an approval nonce: %w", err)
	}
	now = now.UTC().Truncate(time.Second)
	r := Request{
		Act:       act,
		Nonce:     hex.EncodeToString(b[:]),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		State:     StatePending,
	}
	r.Challenge = challengeText(act, r.Nonce, r.ExpiresAt)
	res, err := tx.ExecContext(ctx, `INSERT INTO approval_requests (kind, subject, nonce, challenge, created_at, expires_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(act.Kind), act.Subject, r.Nonce, r.Challenge, r.CreatedAt.Format(timeLayout), r.ExpiresAt.Format(timeLayout), StatePending)
	if err != nil {
		return Request{}, fmt.Errorf("store the approval request: %w", err)
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return Request{}, fmt.Errorf("store the approval request: %w", err)
	}
	return r, nil
}

// Get reads the request id.
func Get(ctx context.Context, tx *sql.Tx, id int64) (Request, error) {
	var (
		r                Request
		kind             string
		created, expires string
		fp               sql.NullString
	)
	err := tx.QueryRowContext(ctx, `SELECT id, kind, subject, nonce, challenge, created_at, expires_at, state, key_fingerprint
		FROM approval_requests WHERE id = ?`, id).
		Scan(&r.ID, &kind, &r.Subject, &r.Nonce, &r.Challenge, &created, &expires, &r.State, &fp)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, fmt.Errorf("%w: #%d", ErrUnknownRequest, id)
	}
	if err != nil {
		return Request{}, fmt.Errorf("read approval request #%d: %w", id, err)
	}
	r.Kind = Kind(kind)
	r.KeyFingerprint = fp.String
	if r.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return Request{}, fmt.Errorf("approval request #%d: created_at: %w", id, err)
	}
	if r.ExpiresAt, err = time.Parse(timeLayout, expires); err != nil {
		return Request{}, fmt.Errorf("approval request #%d: expires_at: %w", id, err)
	}
	return r, nil
}

// Approve answers request id for act with signature, an armored SSHSIG
// signature over the request's stored challenge by one of keys, and on
// success marks the request approved by that key, once: a second answer
// to the same request, or the same signature offered for another, is
// refused as reused. Every refusal wraps one of the Err values above and
// writes nothing, so the caller's transaction may roll back freely; an
// expired request stays pending in the store until Expire marks it.
//
// The order of the checks is the order of the refusals' precedence: the
// request and its act first, then its state and expiry, then the keys and
// the signature. A token, of any role, is not a signature and is refused
// as one.
func Approve(ctx context.Context, tx *sql.Tx, keys []Key, id int64, act Act, signature string, now time.Time) (Request, error) {
	r, err := Get(ctx, tx, id)
	if err != nil {
		return Request{}, err
	}
	if r.Act != act {
		return Request{}, fmt.Errorf("%w: request #%d is for %s, not %s", ErrWrongAct, id, r.Act, act)
	}
	switch r.State {
	case StateApproved:
		return Request{}, fmt.Errorf("%w: request #%d is already approved", ErrReused, id)
	case StateExpired:
		return Request{}, fmt.Errorf("%w: request #%d expired at %s; ask for a fresh challenge", ErrExpired, id, r.ExpiresAt.Format(time.RFC3339))
	}
	if !now.Before(r.ExpiresAt) {
		return Request{}, fmt.Errorf("%w: request #%d expired at %s; ask for a fresh challenge", ErrExpired, id, r.ExpiresAt.Format(time.RFC3339))
	}
	if len(keys) == 0 {
		return Request{}, fmt.Errorf("%w: enroll one with brigada approval add", ErrNoKey)
	}
	if strings.TrimSpace(signature) == "" {
		return Request{}, fmt.Errorf("%w: sign the challenge with ssh-keygen -Y sign -n %s", ErrNoSignature, Namespace)
	}
	sig, err := parseArmored(signature)
	if err != nil {
		return Request{}, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	// The digest is of the decoded blob, not the armor, so rewrapping
	// the same signature's lines does not make it new.
	sum := sha256.Sum256(sig.raw)
	digest := hex.EncodeToString(sum[:])
	var used int64
	switch err := tx.QueryRowContext(ctx, "SELECT id FROM approval_requests WHERE signature_sha256 = ?", digest).Scan(&used); {
	case err == nil:
		return Request{}, fmt.Errorf("%w: that signature already approved request #%d", ErrReused, used)
	case !errors.Is(err, sql.ErrNoRows):
		return Request{}, fmt.Errorf("read approval requests: %w", err)
	}
	key, err := verify(keys, r.Challenge, sig)
	if err != nil {
		return Request{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE approval_requests SET state = ?, key_fingerprint = ?, signature_sha256 = ?, decided_at = ?
		WHERE id = ? AND state = ?`,
		StateApproved, key.Fingerprint, digest, now.UTC().Format(timeLayout), id, StatePending)
	if err != nil {
		return Request{}, fmt.Errorf("record the approval of request #%d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return Request{}, fmt.Errorf("record the approval of request #%d: %w", id, err)
	} else if n != 1 {
		return Request{}, fmt.Errorf("%w: request #%d was answered meanwhile", ErrReused, id)
	}
	r.State = StateApproved
	r.KeyFingerprint = key.Fingerprint
	return r, nil
}

// verify checks sig against challenge and returns the enrolled key that
// made it. The key embedded in the signature only picks the candidate:
// the signature is verified with the enrolled key itself.
func verify(keys []Key, challenge string, sig *sshsig) (Key, error) {
	if sig.namespace != Namespace {
		return Key{}, fmt.Errorf("%w: signed under namespace %q, want %q (ssh-keygen -Y sign -n %s)", ErrBadSignature, sig.namespace, Namespace, Namespace)
	}
	var key *Key
	for i := range keys {
		if string(keys[i].blob) == string(sig.publicKey) {
			key = &keys[i]
			break
		}
	}
	if key == nil {
		return Key{}, fmt.Errorf("%w: %s", ErrUnknownKey, fingerprint(sig.publicKey))
	}
	data, err := signedData(sig.namespace, sig.reserved, sig.hashAlg, []byte(challenge))
	if err != nil {
		return Key{}, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	if err := key.pub.verify(data, sig.signature); err != nil {
		return Key{}, fmt.Errorf("%w: %v (key %s)", ErrBadSignature, err, key.Label)
	}
	return *key, nil
}

// Expire marks every pending request whose expiry has passed by now as
// expired and returns how many it marked. A pending request past its
// expiry already refuses; this only makes the store say so.
func Expire(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	stamp := now.UTC().Format(timeLayout)
	res, err := tx.ExecContext(ctx, "UPDATE approval_requests SET state = ?, decided_at = ? WHERE state = ? AND expires_at <= ?",
		StateExpired, stamp, StatePending, stamp)
	if err != nil {
		return 0, fmt.Errorf("expire approval requests: %w", err)
	}
	return res.RowsAffected()
}
