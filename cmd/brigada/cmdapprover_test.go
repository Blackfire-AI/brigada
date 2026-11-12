package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

func approverPublicKey(t *testing.T) string {
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
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " user@host\n"
}

func runApproverCmd(t *testing.T, state string, args ...string) (int, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	var out, errb bytes.Buffer
	full := append([]string{}, args...)
	if len(args) > 1 { // a bare dispatcher takes no flags
		full = append(full, "--state-dir", state)
	}
	code := run(context.Background(), full, &out, &errb)
	return code, out.String(), errb.String()
}

// everyFileIs0600 asserts the mode of each regular file under dir, and that
// the directory itself is private.
func everyFileIs0600(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Mode().Perm()&0o077 != 0 && path != dir {
				t.Errorf("directory %s mode %o is open to others", path, info.Mode().Perm())
			}
			return nil
		}
		n++
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", path, got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestApproverAddListRemove(t *testing.T) {
	state := filepath.Join(testsupport.ShortStateDir(t), "fresh") // not yet created
	keyFile := filepath.Join(t.TempDir(), "id.pub")
	if err := os.WriteFile(keyFile, []byte(approverPublicKey(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, out, errs := runApproverCmd(t, state, "approver", "list"); code != 0 || !strings.Contains(out, "no approval keys") {
		t.Fatalf("list of nothing: %d %q %q", code, out, errs)
	}
	if code, out, errs := runApproverCmd(t, state, "approver", "add", "--label", "laptop", "--public-key", keyFile); code != 0 || !strings.Contains(out, "enrolled approval key laptop") {
		t.Fatalf("add: %d %q %q", code, out, errs)
	}
	if n := everyFileIs0600(t, state); n != 1 {
		t.Errorf("add wrote %d files, want only the key file", n)
	}
	code, out, errs := runApproverCmd(t, state, "approver", "list")
	if code != 0 || !strings.Contains(out, "laptop") || !strings.Contains(out, "ssh-ed25519") || strings.Contains(out, "user@host") {
		t.Fatalf("list: %d %q %q", code, out, errs)
	}
	if code, _, errs := runApproverCmd(t, state, "approver", "add", "--label", "laptop", "--public-key", keyFile); code != 1 || !strings.Contains(errs, "already enrolled") {
		t.Errorf("duplicate add: %d %q", code, errs)
	}
	if code, out, errs := runApproverCmd(t, state, "approver", "remove", "--label", "laptop"); code != 0 || !strings.Contains(out, "removed approval key laptop") {
		t.Fatalf("remove: %d %q %q", code, out, errs)
	}
	everyFileIs0600(t, state)
	if code, _, errs := runApproverCmd(t, state, "approver", "remove", "--label", "laptop"); code != 1 || !strings.Contains(errs, "no approval key") {
		t.Errorf("second remove: %d %q", code, errs)
	}
	if code, out, _ := runApproverCmd(t, state, "approver", "list"); code != 0 || !strings.Contains(out, "no approval keys") {
		t.Errorf("list after remove: %d %q", code, out)
	}
}

func TestApproverUsageErrors(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	keyFile := filepath.Join(t.TempDir(), "id.pub")
	if err := os.WriteFile(keyFile, []byte(approverPublicKey(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	notAKey := filepath.Join(t.TempDir(), "notakey")
	if err := os.WriteFile(notAKey, []byte("this is not a key\nSECRET-LINE"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		args []string
		code int
		want string
	}{
		"no subcommand":    {[]string{"approver"}, 2, "usage: brigada approver add"},
		"bad subcommand":   {[]string{"approver", "enrol"}, 2, "unknown approver subcommand"},
		"misspelled flag":  {[]string{"approver", "add", "--lable", "x", "--public-key", keyFile}, 2, "flag provided but not defined"},
		"missing label":    {[]string{"approver", "add", "--public-key", keyFile}, 2, "needs --label and --public-key"},
		"missing key":      {[]string{"approver", "add", "--label", "x"}, 2, "needs --label and --public-key"},
		"remove no label":  {[]string{"approver", "remove"}, 2, "needs --label"},
		"positional":       {[]string{"approver", "list", "extra"}, 2, "takes no positional"},
		"bad label":        {[]string{"approver", "add", "--label", "a b", "--public-key", keyFile}, 1, "label"},
		"not a public key": {[]string{"approver", "add", "--label", "x", "--public-key", notAKey}, 1, "not an OpenSSH public key"},
		"unreadable file":  {[]string{"approver", "add", "--label", "x", "--public-key", filepath.Join(state, "nope")}, 1, "read the public key"},
	} {
		t.Run(name, func(t *testing.T) {
			code, _, errs := runApproverCmd(t, state, c.args...)
			if code != c.code || !strings.Contains(errs, c.want) {
				t.Errorf("code=%d stderr=%q, want %d containing %q", code, errs, c.code, c.want)
			}
			if strings.Contains(errs, "SECRET-LINE") {
				t.Errorf("stderr echoed the file's content: %q", errs)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(state, "approval_keys")); err == nil {
		t.Error("a refused add left an enrollment file behind")
	}
}
