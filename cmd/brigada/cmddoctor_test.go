package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorRun runs brigada doctor inside dir.
func doctorRun(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	t.Chdir(dir)
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"doctor"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// doctorRegister makes root look registered: a project.yaml naming the remote.
func doctorRegister(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, ".brigada")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "schema: 1\nrepo: github.com/example/doctor\n"
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorSuccessLeavesNoRef(t *testing.T) {
	root, remote := probeWorkRepo(t)
	doctorRegister(t, root)
	code, out, errb := doctorRun(t, root)
	if code != 0 {
		t.Fatalf("code %d, stdout %q stderr %q", code, out, errb)
	}
	for _, want := range []string{"Git identity: user.name = probe test", "user.email = probe@example.invalid", "Remote: origin", "Push probe: ok", "Harness sign-in:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if refs := wireGit(t, remote, "for-each-ref", initProbeRefPrefix); strings.TrimSpace(refs) != "" {
		t.Fatalf("probe ref left behind: %s", refs)
	}
}

func TestDoctorUnbornHead(t *testing.T) {
	root, remote := probeWorkRepo(t)
	doctorRegister(t, root)
	code, out, errb := doctorRun(t, root)
	if code != 0 || !strings.Contains(out, "Push probe: ok") {
		t.Fatalf("code %d, stdout %q stderr %q", code, out, errb)
	}
	if refs := wireGit(t, remote, "for-each-ref", initProbeRefPrefix); strings.TrimSpace(refs) != "" {
		t.Fatalf("probe ref left behind: %s", refs)
	}
}

func TestDoctorRejectingRemote(t *testing.T) {
	root, remote := probeWorkRepo(t)
	doctorRegister(t, root)
	probeHook(t, remote, "echo \"ERROR: The key you are authenticating with is marked as read only.\" >&2\nexit 1\n")
	code, out, _ := doctorRun(t, root)
	if code != 1 {
		t.Fatalf("code %d, want 1; stdout %q", code, out)
	}
	for _, want := range []string{"Push probe: FAILED", "marked as read only", "Fix: the key or token lacks write access"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorUnregisteredIsUsageError(t *testing.T) {
	root, _ := probeWorkRepo(t)
	code, _, errb := doctorRun(t, root)
	if code != 2 || !strings.Contains(errb, "brigada init") {
		t.Fatalf("code %d, stderr %q; want usage error naming brigada init", code, errb)
	}
}

func TestDoctorExtraArgumentsAreUsageError(t *testing.T) {
	root, _ := probeWorkRepo(t)
	doctorRegister(t, root)
	code, _, errb := doctorRun(t, root, "extra")
	if code != 2 || !strings.Contains(errb, "takes no arguments") {
		t.Fatalf("code %d, stderr %q", code, errb)
	}
	code, _, errb = doctorRun(t, root, "--bogus")
	if code != 2 {
		t.Fatalf("unknown flag: code %d, stderr %q", code, errb)
	}
}
