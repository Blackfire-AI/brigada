package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// pubLine is an ed25519 public key as a .pub file holds it, generated in
// the test; the private half is dropped at once.
func pubLine(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var blob bytes.Buffer
	for _, part := range [][]byte{[]byte("ssh-ed25519"), pub} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(part)))
		blob.Write(n[:])
		blob.Write(part)
	}
	return strings.TrimSpace("ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob.Bytes()) + " " + comment)
}

func TestApprovalAddListRemove(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	state := testsupport.ShortStateDir(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	laptop := write("laptop.pub", pubLine(t, "me@laptop"))
	bare := write("bare.pub", pubLine(t, ""))
	private := write("id_ed25519", "-----BEGIN OPENSSH "+"PRIVATE KEY-----\nAAAA\n-----END OPENSSH "+"PRIVATE KEY-----")
	// The common flag goes before the positional argument.
	cmd := func(sub string, rest ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := run(context.Background(), append([]string{"approval", sub, "--state-dir", state}, rest...), &out, &errb)
		return code, out.String(), errb.String()
	}

	if code, out, _ := cmd("list"); code != 0 || !strings.Contains(out, "no approval keys enrolled") {
		t.Fatalf("list with none = %d %q", code, out)
	}
	if code, out, errs := cmd("add", laptop); code != 0 || !strings.Contains(out, "approval key me@laptop enrolled: ssh-ed25519 SHA256:") {
		t.Fatalf("add = %d %q %q", code, out, errs)
	}
	if code, _, errs := cmd("add", bare); code != 2 || !strings.Contains(errs, "--label") {
		t.Fatalf("add without a label = %d %q, want a usage error naming --label", code, errs)
	}
	if code, _, errs := cmd("add", "--label", "yubikey", bare); code != 0 {
		t.Fatalf("add --label = %d %q", code, errs)
	}
	if code, _, errs := cmd("add", "--label", "again", laptop); code != 1 || !strings.Contains(errs, "already enrolled") {
		t.Fatalf("second add of one key = %d %q", code, errs)
	}
	if code, _, errs := cmd("add", "--label", "secret", private); code != 1 || !strings.Contains(errs, ".pub") {
		t.Fatalf("add of a private key = %d %q", code, errs)
	}
	info, err := os.Stat(filepath.Join(state, "approval-keys"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("approval-keys = %v, %v; want mode 0600", info, err)
	}
	code, out, _ := cmd("list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "me@laptop\tssh-ed25519\tSHA256:") || !strings.HasPrefix(lines[1], "yubikey\t") {
		t.Fatalf("list = %d %q", code, out)
	}
	fp := strings.Split(lines[1], "\t")[2]
	if code, out, errs := cmd("remove", fp); code != 0 || !strings.Contains(out, "approval key yubikey removed") {
		t.Fatalf("remove by fingerprint = %d %q %q", code, out, errs)
	}
	if code, _, _ := cmd("remove", "yubikey"); code != 1 {
		t.Fatalf("remove of a removed key = %d, want 1", code)
	}
	if code, _, _ := cmd("remove", "me@laptop"); code != 0 {
		t.Fatalf("remove by label = %d", code)
	}
	if code, out, _ := cmd("list"); code != 0 || !strings.Contains(out, "no approval keys enrolled") {
		t.Fatalf("list after removing both = %d %q", code, out)
	}
	if code, _, _ := cmd("remove"); code != 2 {
		t.Fatalf("remove with no name = %d, want 2", code)
	}
}
