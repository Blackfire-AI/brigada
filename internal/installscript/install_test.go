package installscript

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func scriptPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func requireTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is unavailable", name)
	}
	return path
}

func TestInstallerSyntax(t *testing.T) {
	script := scriptPath(t)
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("#!/bin/sh\n")) || !bytes.Contains(data, []byte("\nset -eu\n")) {
		t.Fatal("installer must use #!/bin/sh and set -eu")
	}
	if out, err := exec.Command(requireTool(t, "bash"), "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

func TestInstallerShellcheck(t *testing.T) {
	shellcheck, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck is unavailable; CI enforces shellcheck install.sh")
	}
	if out, err := exec.Command(shellcheck, scriptPath(t)).CombinedOutput(); err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}
}

func TestDetectPlatform(t *testing.T) {
	sh := requireTool(t, "sh")
	for _, tc := range []struct{ os, cpu, want string }{
		{"Linux", "x86_64", "linux-amd64"},
		{"Linux", "aarch64", "linux-arm64"},
		{"Darwin", "amd64", "darwin-amd64"},
		{"Darwin", "arm64", "darwin-arm64"},
		{"FreeBSD", "amd64", "unsupported OS"},
		{"Linux", "i686", "unsupported CPU"},
	} {
		t.Run(tc.os+"-"+tc.cpu, func(t *testing.T) {
			cmd := exec.Command(sh, "-c", `. "$1"; detect_platform "$2" "$3"`, "sh", scriptPath(t), tc.os, tc.cpu)
			cmd.Env = append(os.Environ(), "BRIGADA_INSTALL_SOURCE_ONLY=1")
			out, err := cmd.CombinedOutput()
			unsupported := strings.HasPrefix(tc.want, "unsupported")
			if (err != nil) != unsupported || !strings.Contains(string(out), tc.want) {
				t.Fatalf("got %q, %v; want %q (failure=%v)", out, err, tc.want, unsupported)
			}
		})
	}
}

func TestInstall(t *testing.T) {
	sh := requireTool(t, "sh")
	requireTool(t, "tar")
	// curl supports file:// fixture URLs without a network listener.
	requireTool(t, "curl")
	if _, err := exec.LookPath("sha256sum"); err != nil {
		requireTool(t, "shasum")
	}
	platformCmd := exec.Command(sh, "-c", `. "$1"; detect_platform "$(uname -s)" "$(uname -m)"`, "sh", scriptPath(t))
	platformCmd.Env = append(os.Environ(), "BRIGADA_INSTALL_SOURCE_ONLY=1")
	platform, err := platformCmd.CombinedOutput()
	if err != nil {
		t.Skipf("host platform is unsupported: %s", platform)
	}
	asset := "brigada-" + strings.TrimSpace(string(platform)) + ".tar.gz"
	binary := []byte("#!/bin/sh\n[ \"$1\" = version ] || exit 1\nprintf '1.2.3\\n'\n")
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "brigada", Mode: 0755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sums := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive.Bytes()), asset)
	for _, tc := range []struct {
		name, version   string
		tamper, missing bool
	}{
		{name: "latest"},
		{name: "pinned", version: "1.2.3"},
		{name: "tag", version: "v1.2.3"},
		{name: "tampered", tamper: true},
		{name: "missing checksum", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			release := "/latest/download/"
			if tc.version != "" {
				release = "/download/v1.2.3/"
			}
			dir := filepath.Join(root, filepath.FromSlash(release))
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			data := bytes.Clone(archive.Bytes())
			if tc.tamper {
				data = append(data, []byte("tampered")...)
			}
			checksumData := sums
			if tc.missing {
				checksumData = ""
			}
			for name, contents := range map[string][]byte{asset: data, "SHA256SUMS": []byte(checksumData)} {
				if err := os.WriteFile(filepath.Join(dir, name), contents, 0644); err != nil {
					t.Fatal(err)
				}
			}
			baseURL := (&url.URL{Scheme: "file", Path: root}).String()
			home := t.TempDir()
			installDir := filepath.Join(home, ".local", "bin")
			tmp := t.TempDir()
			cmd := exec.Command(sh, scriptPath(t))
			cmd.Env = append(os.Environ(), "HOME="+home, "TMPDIR="+tmp,
				"BRIGADA_BASE_URL="+baseURL, "BRIGADA_VERSION="+tc.version,
				"BRIGADA_INSTALL_SOURCE_ONLY=0", "BRIGADA_INSTALL_DIR=")
			if tc.version != "" {
				installDir = filepath.Join(home, "custom bin")
				cmd.Env = append(cmd.Env, "BRIGADA_INSTALL_DIR="+installDir)
			}
			out, err := cmd.CombinedOutput()
			if tc.tamper || tc.missing {
				want := "checksum mismatch"
				if tc.missing {
					want = "missing checksum"
				}
				if err == nil || !strings.Contains(string(out), want) {
					t.Fatalf("got %q, %v; want %s failure", out, err, want)
				}
				if _, err := os.Stat(filepath.Join(installDir, "brigada")); !os.IsNotExist(err) {
					t.Fatalf("failed verification installed a binary: %v", err)
				}
			} else {
				if err != nil || !strings.HasSuffix(string(out), "1.2.3\n") || !strings.Contains(string(out), "is not on PATH") {
					t.Fatalf("install: %v\n%s", err, out)
				}
				got, err := os.ReadFile(filepath.Join(installDir, "brigada"))
				if err != nil || !bytes.Equal(got, binary) {
					t.Fatalf("installed binary: %q, %v", got, err)
				}
			}
			entries, err := os.ReadDir(tmp)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files not cleaned up: %v, %v", entries, err)
			}
		})
	}
}
