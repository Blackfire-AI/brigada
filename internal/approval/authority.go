package approval

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The kinds of act an approval can name.
const (
	KindPlan   = "plan"
	KindTask   = "task"
	KindPush   = "push"
	KindImport = "import"
)

const (
	// DefaultTTL is a challenge's life when the caller asks for none; MaxTTL
	// is the longest one the authority issues.
	DefaultTTL = 5 * time.Minute
	MaxTTL     = 15 * time.Minute

	keygenTimeout = 15 * time.Second
)

var noncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Act is the one thing an approval is for: its kind and the identity it
// names, a plan, task, push or import id.
type Act struct {
	Kind string
	ID   string
}

func (a Act) validate() error {
	switch a.Kind {
	case KindPlan, KindTask, KindPush, KindImport:
	default:
		return fmt.Errorf("%w: unknown act kind %q", ErrBadChallenge, a.Kind)
	}
	if a.ID == "" || len(a.ID) > 256 {
		return fmt.Errorf("%w: the act's identity is empty or too long", ErrBadChallenge)
	}
	for _, r := range a.ID {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%w: the act's identity holds a space or control character", ErrBadChallenge)
		}
	}
	return nil
}

func (a Act) String() string { return a.Kind + " " + a.ID }

// Challenge is what the user signs for one act. The caller keeps it with
// the pending request and hands it back, with the signature, to Verify.
type Challenge struct {
	Act     Act
	Nonce   string    // 32 lowercase hex characters
	Expires time.Time // whole seconds
}

// Message is the exact text the user signs under Namespace: it binds the
// act, the nonce and the expiry, so a signature for one challenge is
// worthless for any other.
func (c Challenge) Message() ([]byte, error) {
	if err := c.Act.validate(); err != nil {
		return nil, err
	}
	if !noncePattern.MatchString(c.Nonce) {
		return nil, fmt.Errorf("%w: bad nonce", ErrBadChallenge)
	}
	return []byte(fmt.Sprintf("brigada approval v1\nact: %s\nnonce: %s\nexpires: %d\n",
		c.Act, c.Nonce, c.Expires.Unix())), nil
}

// ID names the challenge for spent-marking: a hash of its message.
func (c Challenge) ID() (string, error) {
	msg, err := c.Message()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(msg)
	return hex.EncodeToString(sum[:]), nil
}

// Authority issues challenges and verifies signatures against the keys in
// its Store. It remembers the challenges it accepted, so a second use of
// one is refused; MarkSpent lets a caller seed or persist that memory.
type Authority struct {
	store  *Store
	now    func() time.Time
	keygen string

	mu    sync.Mutex
	spent map[string]time.Time // challenge ID -> its expiry, kept until then
}

// New returns an Authority over store.
func New(store *Store) *Authority { return NewWithClock(store, time.Now) }

// NewWithClock is New with the caller's clock, so an engine under a test
// clock issues and expires challenges by it.
func NewWithClock(store *Store, now func() time.Time) *Authority {
	return &Authority{store: store, now: now, keygen: "ssh-keygen", spent: map[string]time.Time{}}
}

// Issue makes a challenge for act, good for ttl (DefaultTTL when ttl is
// zero, an error above MaxTTL).
func (a *Authority) Issue(act Act, ttl time.Duration) (Challenge, error) {
	if err := act.validate(); err != nil {
		return Challenge{}, err
	}
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < 0 || ttl > MaxTTL {
		return Challenge{}, fmt.Errorf("approval challenge lifetime %s: want up to %s", ttl, MaxTTL)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Challenge{}, fmt.Errorf("mint an approval nonce: %w", err)
	}
	return Challenge{
		Act:     act,
		Nonce:   hex.EncodeToString(b[:]),
		Expires: a.now().Add(ttl).Truncate(time.Second),
	}, nil
}

// MarkSpent records that c was answered, so Verify refuses it from now on.
// A caller with durable pending-request state calls it for each challenge
// it finds spent after a restart. It is kept until c would have expired
// anyway, when the expiry check refuses it.
func (a *Authority) MarkSpent(c Challenge) error {
	id, err := c.ID()
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.spent[id] = c.Expires
	return nil
}

// Spent reports whether c was marked spent or accepted.
func (a *Authority) Spent(c Challenge) bool {
	id, err := c.ID()
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.spent[id]
	return ok
}

// Verify decides whether sig, an armored ssh-keygen signature, approves
// act for challenge c. It returns the label of the enrolled key that
// signed, or an error wrapping one reason of approval.go. Every doubt is a
// refusal. On success the challenge is spent: a second Verify of it, with
// the same signature or any other, is ErrReused.
func (a *Authority) Verify(c Challenge, act Act, sig []byte) (string, error) {
	msg, err := c.Message()
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(sig)) == 0 {
		return "", fmt.Errorf("refusing %s: %w", act, ErrNoSignature)
	}
	if c.Act != act {
		return "", fmt.Errorf("refusing %s: %w (it names %s)", act, ErrWrongAct, c.Act)
	}
	now := a.now()
	if !now.Before(c.Expires) {
		return "", fmt.Errorf("refusing %s: %w (at %s)", act, ErrExpired, c.Expires.UTC().Format(time.RFC3339))
	}
	keys, err := a.store.List()
	if err != nil {
		return "", fmt.Errorf("refusing %s: %w", act, err)
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("refusing %s: %w; enroll one in %s", act, ErrNoKey, a.store.Path())
	}
	id, _ := c.ID()

	// The lock spans the check and the mark, so two callers racing with one
	// signature cannot both win.
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, exp := range a.spent {
		if !now.Before(exp) {
			delete(a.spent, k)
		}
	}
	if _, ok := a.spent[id]; ok {
		return "", fmt.Errorf("refusing %s: %w", act, ErrReused)
	}
	label, err := a.check(keys, msg, sig)
	if err != nil {
		return "", fmt.Errorf("refusing %s: %w", act, err)
	}
	a.spent[id] = c.Expires
	return label, nil
}

// check runs `ssh-keygen -Y verify` for each enrolled key in turn against a
// private temporary allowed_signers file.
func (a *Authority) check(keys []Key, msg, sig []byte) (string, error) {
	bin, err := exec.LookPath(a.keygen)
	if err != nil {
		return "", fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	dir, err := os.MkdirTemp("", "brigada-approval-")
	if err != nil {
		return "", fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	defer os.RemoveAll(dir)
	signers := filepath.Join(dir, "allowed_signers")
	sigFile := filepath.Join(dir, "message.sig")
	var lines strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&lines, "%s namespaces=%q %s\n", k.Label, Namespace, k.PublicKey)
	}
	if err := os.WriteFile(signers, []byte(lines.String()), 0o600); err != nil {
		return "", fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	if err := os.WriteFile(sigFile, sig, 0o600); err != nil {
		return "", fmt.Errorf("%w (%v)", ErrUnavailable, err)
	}
	var last string
	for _, k := range keys {
		ctx, cancel := context.WithTimeout(context.Background(), keygenTimeout)
		cmd := exec.CommandContext(ctx, bin, "-Y", "verify", "-f", signers, "-I", k.Label, "-n", Namespace, "-s", sigFile)
		cmd.Stdin = bytes.NewReader(msg)
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			return k.Label, nil
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return "", fmt.Errorf("%w (%v)", ErrUnavailable, err)
		}
		last = strings.TrimSpace(string(out))
	}
	return "", fmt.Errorf("%w: %s", ErrBadSignature, last)
}
