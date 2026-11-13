package approval

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// fakePublicKey builds a well-formed ed25519 public key line without
// ssh-keygen, for the tests that need an enrolled key but no signature.
func fakePublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var blob []byte
	for _, part := range [][]byte{[]byte("ssh-ed25519"), pub} {
		blob = binary.BigEndian.AppendUint32(blob, uint32(len(part)))
		blob = append(blob, part...)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
}

func requireKeygen(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Fatalf("ssh-keygen is required on the check host for the approval tests: %v", err)
	}
}

// signer is a key the test generated with ssh-keygen.
type signer struct {
	private string
	public  string
}

func newSigner(t *testing.T) signer {
	t.Helper()
	requireKeygen(t)
	private := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", private).CombinedOutput(); err != nil {
		t.Fatalf("generate a key: %v: %s", err, out)
	}
	pub, err := os.ReadFile(private + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return signer{private: private, public: string(pub)}
}

func (s signer) sign(t *testing.T, namespace string, msg []byte) []byte {
	t.Helper()
	file := filepath.Join(t.TempDir(), "msg")
	if err := os.WriteFile(file, msg, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("ssh-keygen", "-Y", "sign", "-f", s.private, "-n", namespace, file).CombinedOutput(); err != nil {
		t.Fatalf("sign: %v: %s", err, out)
	}
	sig, err := os.ReadFile(file + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func (s signer) signChallenge(t *testing.T, c Challenge) []byte {
	t.Helper()
	msg, err := c.Message()
	if err != nil {
		t.Fatal(err)
	}
	return s.sign(t, Namespace, msg)
}

// enrolled returns an authority whose store holds s's key, and a clock the
// test moves.
func enrolled(t *testing.T, s signer) (*Authority, *time.Time) {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), StoreFile))
	if err := store.Add(Key{Label: "me", PublicKey: s.public}); err != nil {
		t.Fatal(err)
	}
	a := New(store)
	clock := time.Now()
	a.now = func() time.Time { return clock }
	return a, &clock
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s := newSigner(t)
	a, _ := enrolled(t, s)
	act := Act{Kind: KindPlan, ID: "plan-12"}
	c, err := a.Issue(act, 0)
	if err != nil {
		t.Fatal(err)
	}
	label, err := a.Verify(c, act, s.signChallenge(t, c))
	if err != nil {
		t.Fatalf("a good signature was refused: %v", err)
	}
	if label != "me" {
		t.Errorf("signing key label = %q, want me", label)
	}
}

func TestExpiredChallengeIsRefused(t *testing.T) {
	s := newSigner(t)
	a, clock := enrolled(t, s)
	act := Act{Kind: KindTask, ID: "t-1"}
	c, err := a.Issue(act, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sig := s.signChallenge(t, c)
	*clock = clock.Add(2 * time.Minute)
	if _, err := a.Verify(c, act, sig); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired challenge: err = %v, want ErrExpired", err)
	}
}

func TestChallengeForOneActIsRefusedForAnother(t *testing.T) {
	s := newSigner(t)
	a, _ := enrolled(t, s)
	planAct := Act{Kind: KindPlan, ID: "7"}
	c, err := a.Issue(planAct, 0)
	if err != nil {
		t.Fatal(err)
	}
	sig := s.signChallenge(t, c)

	// The caller asks to approve another act with this challenge.
	if _, err := a.Verify(c, Act{Kind: KindPush, ID: "7"}, sig); !errors.Is(err, ErrWrongAct) {
		t.Errorf("challenge for a plan used for a push: err = %v, want ErrWrongAct", err)
	}
	// The caller swaps in another act's challenge but keeps this signature.
	otherAct := Act{Kind: KindPlan, ID: "8"}
	other, err := a.Issue(otherAct, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Verify(other, otherAct, sig); !errors.Is(err, ErrBadSignature) {
		t.Errorf("signature of one challenge on another: err = %v, want ErrBadSignature", err)
	}
	// A signature made under another namespace (a git commit, say) approves nothing.
	msg, _ := c.Message()
	foreign := s.sign(t, "git", msg)
	if _, err := a.Verify(c, planAct, foreign); !errors.Is(err, ErrBadSignature) {
		t.Errorf("signature under another namespace: err = %v, want ErrBadSignature", err)
	}
	// And the honest pair still works: the refusals spent nothing.
	if _, err := a.Verify(c, planAct, sig); err != nil {
		t.Errorf("honest signature after refusals: %v", err)
	}
}

func TestReusedSignatureIsRefused(t *testing.T) {
	s := newSigner(t)
	a, _ := enrolled(t, s)
	act := Act{Kind: KindImport, ID: "imp-3"}
	c, err := a.Issue(act, 0)
	if err != nil {
		t.Fatal(err)
	}
	sig := s.signChallenge(t, c)
	if _, err := a.Verify(c, act, sig); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Verify(c, act, sig); !errors.Is(err, ErrReused) {
		t.Fatalf("second use: err = %v, want ErrReused", err)
	}
	// A fresh signature of the same challenge is no way round it.
	if _, err := a.Verify(c, act, s.signChallenge(t, c)); !errors.Is(err, ErrReused) {
		t.Fatalf("re-signed challenge: err = %v, want ErrReused", err)
	}
}

func TestMarkSpentRefusesAChallengeNeverAccepted(t *testing.T) {
	s := newSigner(t)
	a, _ := enrolled(t, s)
	act := Act{Kind: KindPlan, ID: "9"}
	c, err := a.Issue(act, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.MarkSpent(c); err != nil {
		t.Fatal(err)
	}
	if !a.Spent(c) {
		t.Error("Spent = false after MarkSpent")
	}
	if _, err := a.Verify(c, act, s.signChallenge(t, c)); !errors.Is(err, ErrReused) {
		t.Fatalf("marked-spent challenge: err = %v, want ErrReused", err)
	}
}

func TestClientTokenAloneSatisfiesNothing(t *testing.T) {
	act := Act{Kind: KindPlan, ID: "1"}
	token := []byte("cli client 0123456789abcdef0123456789abcdef0123")

	t.Run("no key enrolled", func(t *testing.T) {
		a := New(NewStore(filepath.Join(t.TempDir(), StoreFile)))
		c, err := a.Issue(act, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Verify(c, act, token); !errors.Is(err, ErrNoKey) {
			t.Errorf("token as signature, no key: err = %v, want ErrNoKey", err)
		}
	})
	t.Run("no signature", func(t *testing.T) {
		a := New(NewStore(filepath.Join(t.TempDir(), StoreFile)))
		c, _ := a.Issue(act, 0)
		if _, err := a.Verify(c, act, nil); !errors.Is(err, ErrNoSignature) {
			t.Errorf("no signature: err = %v, want ErrNoSignature", err)
		}
	})
	t.Run("key enrolled", func(t *testing.T) {
		s := newSigner(t)
		a, _ := enrolled(t, s)
		c, _ := a.Issue(act, 0)
		if _, err := a.Verify(c, act, token); !errors.Is(err, ErrBadSignature) {
			t.Errorf("token as signature: err = %v, want ErrBadSignature", err)
		}
	})
}

func TestMissingKeygenFailsClosed(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), StoreFile))
	if err := store.Add(Key{Label: "me", PublicKey: fakePublicKey(t)}); err != nil {
		t.Fatal(err)
	}
	a := New(store)
	a.keygen = "brigada-no-such-ssh-keygen"
	act := Act{Kind: KindTask, ID: "x"}
	c, _ := a.Issue(act, 0)
	if _, err := a.Verify(c, act, []byte("a signature")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing ssh-keygen: err = %v, want ErrUnavailable", err)
	}
}

func TestMalformedChallengeIsRefused(t *testing.T) {
	a := New(NewStore(filepath.Join(t.TempDir(), StoreFile)))
	if _, err := a.Issue(Act{Kind: "merge", ID: "1"}, 0); !errors.Is(err, ErrBadChallenge) {
		t.Errorf("unknown kind: err = %v, want ErrBadChallenge", err)
	}
	if _, err := a.Issue(Act{Kind: KindPlan, ID: "1\nact: push 2"}, 0); !errors.Is(err, ErrBadChallenge) {
		t.Errorf("newline in identity: err = %v, want ErrBadChallenge", err)
	}
	if _, err := a.Issue(Act{Kind: KindPlan, ID: "1"}, time.Hour); err == nil {
		t.Error("an hour-long challenge was issued")
	}
	act := Act{Kind: KindPlan, ID: "1"}
	if _, err := a.Verify(Challenge{Act: act, Nonce: "nope"}, act, []byte("sig")); !errors.Is(err, ErrBadChallenge) {
		t.Errorf("bad nonce: err = %v, want ErrBadChallenge", err)
	}
}

func TestStoreAddListRemove(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), StoreFile))
	if keys, err := store.List(); err != nil || len(keys) != 0 {
		t.Fatalf("empty store: %v, %v", keys, err)
	}
	pub := fakePublicKey(t)
	if err := store.Add(Key{Label: "work", PublicKey: pub + " comment ignored"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(Key{Label: "work", PublicKey: pub}); !errors.Is(err, ErrKeyExists) {
		t.Errorf("duplicate label: err = %v, want ErrKeyExists", err)
	}
	if err := store.Add(Key{Label: "home", PublicKey: fakePublicKey(t)}); err != nil {
		t.Fatal(err)
	}
	keys, err := store.List()
	if err != nil || len(keys) != 2 || keys[0].Label != "home" || keys[1].PublicKey != pub {
		t.Fatalf("List = %v, %v", keys, err)
	}
	if err := store.Remove("work"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("work"); !errors.Is(err, ErrNoSuchKey) {
		t.Errorf("second remove: err = %v, want ErrNoSuchKey", err)
	}
	if keys, _ := store.List(); len(keys) != 1 || keys[0].Label != "home" {
		t.Errorf("after remove: %v", keys)
	}
}

func TestParseKeyRefusesWhatIsNotAPublicKey(t *testing.T) {
	good := fakePublicKey(t)
	for name, c := range map[string]struct{ label, pub string }{
		"armored key text":   {"k", "-----BEGIN OPENSSH-----"},
		"options field":      {"k", `command="x" ` + good},
		"type blob mismatch": {"k", "ssh-rsa " + good[len("ssh-ed25519 "):]},
		"bad label":          {"a b", good},
		"empty label":        {"", good},
	} {
		if _, err := ParseKey(c.label, c.pub); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Never skips: it needs no ssh-keygen, only the filesystem.
func TestEnrollmentFileIsMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), StoreFile)
	// A file that already exists loose must come out tight.
	if err := os.WriteFile(path, []byte("# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewStore(path)
	if err := store.Add(Key{Label: "me", PublicKey: fakePublicKey(t)}); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("enrollment file mode %s = %o, want 600", when, got)
		}
	}
	check("after add")
	if err := store.Add(Key{Label: "two", PublicKey: fakePublicKey(t)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("two"); err != nil {
		t.Fatal(err)
	}
	check("after remove")
}
