package approval

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// signer is a key generated in the test, signing as ssh-keygen -Y sign
// would. The package itself never signs: it holds no private key.
type signer struct {
	blob []byte // the public key's wire blob
	sign func(data []byte) []byte
}

func mpint(b *bytes.Buffer, n *big.Int) {
	v := n.Bytes()
	if len(v) > 0 && v[0]&0x80 != 0 {
		v = append([]byte{0}, v...)
	}
	putString(b, v)
}

func wireOf(parts ...[]byte) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		putString(&b, p)
	}
	return b.Bytes()
}

func newEd25519(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{
		blob: wireOf([]byte(typeEd25519), pub),
		sign: func(data []byte) []byte { return wireOf([]byte(typeEd25519), ed25519.Sign(priv, data)) },
	}
}

// newSKEd25519 stands in for a security key: the signature covers the
// application, the flags and counter, and the data's hash.
func newSKEd25519(t *testing.T, flags byte) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const app = "ssh:"
	return signer{
		blob: wireOf([]byte(typeSKEd25519), pub, []byte(app)),
		sign: func(data []byte) []byte {
			appHash := sha256.Sum256([]byte(app))
			msg := sha256.Sum256(data)
			var signed bytes.Buffer
			signed.Write(appHash[:])
			signed.WriteByte(flags)
			binary.Write(&signed, binary.BigEndian, uint32(7))
			signed.Write(msg[:])
			var b bytes.Buffer
			putString(&b, []byte(typeSKEd25519))
			putString(&b, ed25519.Sign(priv, signed.Bytes()))
			b.WriteByte(flags)
			binary.Write(&b, binary.BigEndian, uint32(7))
			return b.Bytes()
		},
	}
}

func newECDSA(t *testing.T) signer {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	point, err := priv.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return signer{
		blob: wireOf([]byte(typeECDSA256), []byte("nistp256"), point),
		sign: func(data []byte) []byte {
			h := sha256.Sum256(data)
			r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
			if err != nil {
				t.Fatal(err)
			}
			var rs bytes.Buffer
			mpint(&rs, r)
			mpint(&rs, s)
			return wireOf([]byte(typeECDSA256), rs.Bytes())
		},
	}
}

func newRSA(t *testing.T) signer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	putString(&blob, []byte(typeRSA))
	mpint(&blob, big.NewInt(int64(priv.E)))
	mpint(&blob, priv.N)
	return signer{
		blob: blob.Bytes(),
		sign: func(data []byte) []byte {
			h := sha512.Sum512(data)
			sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA512, h[:])
			if err != nil {
				t.Fatal(err)
			}
			return wireOf([]byte("rsa-sha2-512"), sig)
		},
	}
}

// line is the key's authorized_keys line, as a .pub file holds it.
func (s signer) line(comment string) string {
	typ := string((&wire{b: s.blob}).str())
	return strings.TrimSpace(typ + " " + base64.StdEncoding.EncodeToString(s.blob) + " " + comment)
}

// signAs makes the armored SSHSIG signature of message under namespace.
func (s signer) signAs(t *testing.T, namespace string, message string) string {
	t.Helper()
	data, err := signedData(namespace, nil, "sha512", []byte(message))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.WriteString(sigMagic)
	binary.Write(&b, binary.BigEndian, uint32(sigVersion))
	putString(&b, s.blob)
	putString(&b, []byte(namespace))
	putString(&b, nil)
	putString(&b, []byte("sha512"))
	putString(&b, s.sign(data))
	enc := base64.StdEncoding.EncodeToString(b.Bytes())
	var out strings.Builder
	out.WriteString(armorBegin + "\n")
	for len(enc) > 70 {
		out.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	out.WriteString(enc + "\n" + armorEnd + "\n")
	return out.String()
}

func (s signer) signFor(t *testing.T, r Request) string {
	return s.signAs(t, Namespace, r.Challenge)
}

// enroll writes s to a fresh key file and returns the loaded keys.
func enroll(t *testing.T, label string, signers ...signer) (string, []Key) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approval-keys")
	for i, s := range signers {
		key, _, err := ParsePublicKey(s.line(""))
		if err != nil {
			t.Fatalf("parse key %d: %v", i, err)
		}
		if err := AddKey(path, label+string(rune('a'+i)), key); err != nil {
			t.Fatalf("enroll key %d: %v", i, err)
		}
	}
	keys, err := LoadKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, keys
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var t0 = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

func issue(t *testing.T, s *store.Store, act Act) Request {
	t.Helper()
	var r Request
	if err := s.Update(context.Background(), func(tx *sql.Tx) (err error) {
		r, err = Issue(context.Background(), tx, act, t0, DefaultTTL)
		return err
	}); err != nil {
		t.Fatalf("issue %s: %v", act, err)
	}
	return r
}

func approve(s *store.Store, keys []Key, id int64, act Act, sig string, now time.Time) (Request, error) {
	var r Request
	err := s.Update(context.Background(), func(tx *sql.Tx) (err error) {
		r, err = Approve(context.Background(), tx, keys, id, act, sig, now)
		return err
	})
	return r, err
}

func get(t *testing.T, s *store.Store, id int64) Request {
	t.Helper()
	var r Request
	if err := s.View(context.Background(), func(tx *sql.Tx) (err error) {
		r, err = Get(context.Background(), tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

var planAct = Act{Kind: KindPlan, Subject: "plan 7 version 2"}

func TestSignVerifyRoundTrip(t *testing.T) {
	for name, s := range map[string]signer{
		"ed25519":    newEd25519(t),
		"sk-ed25519": newSKEd25519(t, skUserPresent),
		"ecdsa":      newECDSA(t),
		"rsa":        newRSA(t),
	} {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			_, keys := enroll(t, "k", s)
			r := issue(t, st, planAct)
			if !strings.Contains(r.Challenge, "kind: plan\n") || !strings.Contains(r.Challenge, "subject: plan 7 version 2\n") ||
				!strings.Contains(r.Challenge, "nonce: "+r.Nonce+"\n") || !strings.HasSuffix(r.Challenge, "Z\n") {
				t.Fatalf("challenge does not name the act, nonce and expiry: %q", r.Challenge)
			}
			got, err := approve(st, keys, r.ID, planAct, s.signFor(t, r), t0.Add(time.Minute))
			if err != nil {
				t.Fatalf("approve: %v", err)
			}
			if got.State != StateApproved || got.KeyFingerprint != keys[0].Fingerprint {
				t.Fatalf("approved request = %+v, want approved by %s", got, keys[0].Fingerprint)
			}
			if stored := get(t, st, r.ID); stored.State != StateApproved || stored.KeyFingerprint != keys[0].Fingerprint {
				t.Fatalf("stored request = %+v, want approved by the key", stored)
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	enrolled := newEd25519(t)
	stranger := newEd25519(t)
	_, keys := enroll(t, "k", enrolled)
	in := t0.Add(time.Minute)

	cases := []struct {
		name  string
		keys  []Key
		act   Act
		sig   func(r Request) string
		now   time.Time
		want  error
		state string
	}{
		{"no enrolled key", nil, planAct, func(r Request) string { return enrolled.signFor(t, r) }, in, ErrNoKey, StatePending},
		{"no signature", keys, planAct, func(Request) string { return "" }, in, ErrNoSignature, StatePending},
		{"blank signature", keys, planAct, func(Request) string { return " \n\t" }, in, ErrNoSignature, StatePending},
		{"garbage", keys, planAct, func(Request) string { return "yes, approved" }, in, ErrBadSignature, StatePending},
		{"key not enrolled", keys, planAct, func(r Request) string { return stranger.signFor(t, r) }, in, ErrUnknownKey, StatePending},
		{"wrong namespace", keys, planAct, func(r Request) string { return enrolled.signAs(t, "git", r.Challenge) }, in, ErrBadSignature, StatePending},
		{"wrong text", keys, planAct, func(r Request) string { return enrolled.signAs(t, Namespace, r.Challenge+"more") }, in, ErrBadSignature, StatePending},
		{"expired", keys, planAct, func(r Request) string { return enrolled.signFor(t, r) }, t0.Add(DefaultTTL), ErrExpired, StatePending},
		{"another act", keys, Act{Kind: KindPlan, Subject: "plan 8 version 1"}, func(r Request) string { return enrolled.signFor(t, r) }, in, ErrWrongAct, StatePending},
		{"another kind", keys, Act{Kind: KindPush, Subject: planAct.Subject}, func(r Request) string { return enrolled.signFor(t, r) }, in, ErrWrongAct, StatePending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			r := issue(t, st, planAct)
			_, err := approve(st, tc.keys, r.ID, tc.act, tc.sig(r), tc.now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("approve = %v, want %v", err, tc.want)
			}
			if got := get(t, st, r.ID); got.State != tc.state || got.KeyFingerprint != "" {
				t.Fatalf("after a refusal the request is %+v, want it still %s", got, tc.state)
			}
		})
	}
}

func TestUnknownRequestRefused(t *testing.T) {
	s := newEd25519(t)
	_, keys := enroll(t, "k", s)
	st := openStore(t)
	if _, err := approve(st, keys, 99, planAct, "", t0); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("approve of no request = %v, want %v", err, ErrUnknownRequest)
	}
}

// TestChallengeForOneActRefusedForAnother: a signature over the challenge
// of one act approves that act and no other, even when the other act's
// request is pending under the same key.
func TestChallengeForOneActRefusedForAnother(t *testing.T) {
	s := newEd25519(t)
	_, keys := enroll(t, "k", s)
	st := openStore(t)
	land := Act{Kind: KindLand, Subject: "task 3"}
	push := Act{Kind: KindPush, Subject: "project 1"}
	landReq := issue(t, st, land)
	pushReq := issue(t, st, push)
	sig := s.signFor(t, landReq)

	if _, err := approve(st, keys, landReq.ID, push, sig, t0); !errors.Is(err, ErrWrongAct) {
		t.Fatalf("land's request offered for the push = %v, want %v", err, ErrWrongAct)
	}
	if _, err := approve(st, keys, pushReq.ID, push, sig, t0); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("land's signature on the push's challenge = %v, want %v", err, ErrBadSignature)
	}
	if _, err := approve(st, keys, landReq.ID, land, sig, t0); err != nil {
		t.Fatalf("land's signature on its own challenge: %v", err)
	}
	if got := get(t, st, pushReq.ID); got.State != StatePending {
		t.Fatalf("push request = %s, want pending", got.State)
	}
}

func TestReusedSignatureRefused(t *testing.T) {
	s := newEd25519(t)
	_, keys := enroll(t, "k", s)
	st := openStore(t)
	first := issue(t, st, planAct)
	second := issue(t, st, planAct)
	if first.Nonce == second.Nonce || first.Challenge == second.Challenge {
		t.Fatal("two challenges for one act share a nonce")
	}
	sig := s.signFor(t, first)
	if _, err := approve(st, keys, first.ID, planAct, sig, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := approve(st, keys, first.ID, planAct, sig, t0); !errors.Is(err, ErrReused) {
		t.Fatalf("second answer to one request = %v, want %v", err, ErrReused)
	}
	// Rewrapped armor is the same signature.
	rewrapped := strings.ReplaceAll(sig, "\n", "\r\n  ")
	if _, err := approve(st, keys, second.ID, planAct, rewrapped, t0); !errors.Is(err, ErrReused) {
		t.Fatalf("one signature for a second request = %v, want %v", err, ErrReused)
	}
	if _, err := approve(st, keys, second.ID, planAct, s.signFor(t, second), t0); err != nil {
		t.Fatalf("a fresh signature for the second request: %v", err)
	}
}

func TestExpire(t *testing.T) {
	s := newEd25519(t)
	_, keys := enroll(t, "k", s)
	st := openStore(t)
	r := issue(t, st, planAct)
	var n int64
	if err := st.Update(context.Background(), func(tx *sql.Tx) (err error) {
		n, err = Expire(context.Background(), tx, t0.Add(DefaultTTL-time.Second))
		return err
	}); err != nil || n != 0 {
		t.Fatalf("expire before the expiry = %d, %v; want 0", n, err)
	}
	if err := st.Update(context.Background(), func(tx *sql.Tx) (err error) {
		n, err = Expire(context.Background(), tx, t0.Add(DefaultTTL))
		return err
	}); err != nil || n != 1 {
		t.Fatalf("expire at the expiry = %d, %v; want 1", n, err)
	}
	if got := get(t, st, r.ID); got.State != StateExpired {
		t.Fatalf("state = %s, want expired", got.State)
	}
	// Expired stays expired, even for a valid signature at a time inside
	// the original window.
	if _, err := approve(st, keys, r.ID, planAct, s.signFor(t, r), t0); !errors.Is(err, ErrExpired) {
		t.Fatalf("approve of an expired request = %v, want %v", err, ErrExpired)
	}
}

// TestTokenAloneSatisfiesNothing: a client token — the secret a process
// reading the state directory can hold — is no signature, wherever it is
// offered, and the request stays pending.
func TestTokenAloneSatisfiesNothing(t *testing.T) {
	s := newEd25519(t)
	_, keys := enroll(t, "k", s)
	st := openStore(t)
	r := issue(t, st, planAct)
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := lifecycle.EnsureTokenFile(tokens); err != nil {
		t.Fatal(err)
	}
	_, token, err := lifecycle.TokenForRole(tokens, "client")
	if err != nil {
		t.Fatal(err)
	}
	for _, offered := range []string{token, "Bearer " + token, armorBegin + "\n" + token + "\n" + armorEnd} {
		if _, err := approve(st, keys, r.ID, planAct, offered, t0); !errors.Is(err, ErrBadSignature) {
			t.Errorf("token %q offered as the signature = %v, want %v", offered, err, ErrBadSignature)
		}
	}
	// A token enrolled as a key is refused at enrollment.
	if _, _, err := ParsePublicKey("ssh-ed25519 " + token); err == nil {
		t.Error("a token parsed as a public key")
	}
	if got := get(t, st, r.ID); got.State != StatePending {
		t.Fatalf("state = %s, want pending", got.State)
	}
}

func TestSecurityKeyNeedsPresence(t *testing.T) {
	s := newSKEd25519(t, 0) // signed without a touch
	_, keys := enroll(t, "k", s)
	st := openStore(t)
	r := issue(t, st, planAct)
	_, err := approve(st, keys, r.ID, planAct, s.signFor(t, r), t0)
	if !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), "user presence") {
		t.Fatalf("approve without user presence = %v, want a bad signature naming presence", err)
	}
}

func TestIssueRefusesBadActs(t *testing.T) {
	st := openStore(t)
	for _, act := range []Act{{Kind: "merge", Subject: "x"}, {Kind: KindPlan}, {Kind: KindPlan, Subject: "a\nkind: push"}} {
		err := st.Update(context.Background(), func(tx *sql.Tx) error {
			_, err := Issue(context.Background(), tx, act, t0, DefaultTTL)
			return err
		})
		if err == nil {
			t.Errorf("issue %+v succeeded", act)
		}
	}
}

func TestKeyFile(t *testing.T) {
	a, b := newEd25519(t), newECDSA(t)
	path := filepath.Join(t.TempDir(), "approval-keys")
	if keys, err := LoadKeys(path); err != nil || len(keys) != 0 {
		t.Fatalf("missing file = %v, %v; want no keys", keys, err)
	}
	ka, comment, err := ParsePublicKey(a.line("me@laptop") + "\n")
	if err != nil || comment != "me@laptop" {
		t.Fatalf("parse = %q, %v", comment, err)
	}
	kb, _, err := ParsePublicKey(b.line(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := AddKey(path, "laptop", ka); err != nil {
		t.Fatal(err)
	}
	if err := AddKey(path, "laptop", kb); err == nil {
		t.Error("a second key under one label was enrolled")
	}
	if err := AddKey(path, "again", ka); err == nil {
		t.Error("one key was enrolled twice")
	}
	if err := AddKey(path, "two words", kb); err == nil {
		t.Error("a label with a space was enrolled")
	}
	if err := AddKey(path, "yubikey", kb); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	keys, err := LoadKeys(path)
	if err != nil || len(keys) != 2 || keys[0].Label != "laptop" || keys[1].Label != "yubikey" || keys[1].Fingerprint != kb.Fingerprint {
		t.Fatalf("loaded keys = %+v, %v", keys, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatal("the key file holds private material")
	}
	if removed, err := RemoveKey(path, ka.Fingerprint); err != nil || removed.Label != "laptop" {
		t.Fatalf("remove by fingerprint = %+v, %v", removed, err)
	}
	if _, err := RemoveKey(path, "laptop"); err == nil {
		t.Error("removing a removed key succeeded")
	}
	if _, err := RemoveKey(path, "yubikey"); err != nil {
		t.Fatal(err)
	}
	if keys, err := LoadKeys(path); err != nil || len(keys) != 0 {
		t.Fatalf("after removing both = %v, %v", keys, err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeys(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a world-readable key file loaded: %v", err)
	}
}

func TestParsePublicKeyRefusals(t *testing.T) {
	private := "-----BEGIN OPENSSH " + "PRIVATE KEY-----\nAAAA\n-----END OPENSSH " + "PRIVATE KEY-----\n"
	if _, _, err := ParsePublicKey(private); err == nil || !strings.Contains(err.Error(), ".pub") {
		t.Errorf("private key = %v, want a refusal pointing at the .pub file", err)
	}
	a := newEd25519(t)
	dsa := "ssh-dss " + base64.StdEncoding.EncodeToString(wireOf([]byte("ssh-dss"), []byte{1}, []byte{2}, []byte{3}, []byte{4}))
	for name, text := range map[string]string{
		"empty":        "",
		"two keys":     a.line("") + "\n" + a.line(""),
		"type differs": "ecdsa-sha2-nistp256 " + strings.Fields(a.line(""))[1],
		"not base64":   "ssh-ed25519 !!!",
		"dsa":          dsa,
		"short rsa": func() string {
			priv, _ := rsa.GenerateKey(rand.Reader, 1024)
			var blob bytes.Buffer
			putString(&blob, []byte(typeRSA))
			mpint(&blob, big.NewInt(int64(priv.E)))
			mpint(&blob, priv.N)
			return "ssh-rsa " + base64.StdEncoding.EncodeToString(blob.Bytes())
		}(),
	} {
		if _, _, err := ParsePublicKey(text); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
