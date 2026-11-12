package approval

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOpenSSHInterop signs a challenge with OpenSSH's own ssh-keygen and
// verifies it here, so the package and OpenSSH agree on the format, not
// just the package with its own test signer. The key is generated in the
// test and never leaves its temp directory. It is skipped where ssh-keygen
// is not installed.
func TestOpenSSHInterop(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	for _, typ := range [][]string{{"-t", "ed25519"}, {"-t", "ecdsa", "-b", "256"}, {"-t", "ecdsa", "-b", "384"}, {"-t", "rsa", "-b", "3072"}} {
		t.Run(strings.Join(typ[1:], "-"), func(t *testing.T) { interop(t, keygen, typ) })
	}
}

func interop(t *testing.T, keygen string, typ []string) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	args := append([]string{"-q", "-N", "", "-C", "test", "-f", key}, typ...)
	if out, err := exec.Command(keygen, args...).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen %v: %v\n%s", typ, err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	k, comment, err := ParsePublicKey(string(pub))
	if err != nil || comment != "test" {
		t.Fatalf("parse ssh-keygen's public key = %q, %v", comment, err)
	}
	path := filepath.Join(dir, "approval-keys")
	if err := AddKey(path, "test", k); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys(path)
	if err != nil {
		t.Fatal(err)
	}

	st := openStore(t)
	r := issue(t, st, planAct)
	challenge := filepath.Join(dir, "challenge")
	if err := os.WriteFile(challenge, []byte(r.Challenge), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(keygen, "-Y", "sign", "-n", Namespace, "-f", key, challenge).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v\n%s", err, out)
	}
	sig, err := os.ReadFile(challenge + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	got, err := approve(st, keys, r.ID, planAct, string(sig), t0.Add(time.Second))
	if err != nil {
		t.Fatalf("approve with ssh-keygen's signature: %v", err)
	}
	if got.KeyFingerprint != keys[0].Fingerprint {
		t.Fatalf("approved by %s, want %s", got.KeyFingerprint, keys[0].Fingerprint)
	}
	// OpenSSH's fingerprint is ours.
	out, err := exec.Command(keygen, "-l", "-E", "sha256", "-f", key+".pub").Output()
	if err != nil {
		t.Fatalf("ssh-keygen -l: %v", err)
	}
	if want := keys[0].Fingerprint; !strings.Contains(string(out), " "+want+" ") {
		t.Fatalf("ssh-keygen -l printed %q, want fingerprint %s", out, want)
	}
}
