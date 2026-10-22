package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
)

func TestSuspectPath(t *testing.T) {
	for p, want := range map[string]bool{
		".env":                       true,
		"cfg/.env.production":        true,
		"app.env":                    true,
		".env.example":               false,
		"certs/server.pem":           true,
		"tls/Server.KEY":             true,
		"id_rsa":                     true,
		"home/.ssh/id_ed25519":       true,
		"credentials.json":           true,
		"aws/credentials":            true,
		".netrc":                     true,
		".aws/config":                true,
		".local/share/x":             true,
		".gitignore":                 false,
		".github/workflows/ci.yml":   false,
		".golangci.yml":              false,
		"src/main.go":                false,
		"docs/credentials-howto.txt": true,
		"internal/env/env.go":        false,
	} {
		if got := suspectPath(p); got != want {
			t.Errorf("suspectPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestSuspectLeftoversReadsOnlyNewFiles(t *testing.T) {
	// ?? untracked, A staged new, M modified (tracked: never listed), R a
	// rename carrying its source as a second field.
	porcelain := "?? .env\x00?? src/new.go\x00 M .env.prod\x00A  keys/server.pem\x00R  renamed.txt\x00.env.old\x00?? .npmrc\x00R  .envrc\x00tracked.txt\x00"
	got := suspectLeftovers(porcelain)
	want := []string{".env", "keys/server.pem", ".npmrc", ".envrc"}
	if !slices.Equal(got, want) {
		t.Errorf("suspectLeftovers = %q, want %q", got, want)
	}
}

// TestEnsurePushedRefusesCredentialLookingLeftovers: the runner stages
// what the worker left, so a stray env file or key must stop the push and
// be named, while a tracked file's edit and an ordinary new file go.
func TestEnsurePushedRefusesCredentialLookingLeftovers(t *testing.T) {
	ctx := context.Background()
	remote := newRemote(t)
	ws, err := prepareWorkspace(ctx, &Config{StateDir: t.TempDir()}, &api.JobGrant{
		JobID: 23, TaskID: 8, Attempt: 1, RepoURL: remote, BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	defer ws.remove(ctx)

	write := func(name string) {
		t.Helper()
		p := filepath.Join(ws.dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("work.txt")
	write(".env")
	write("deploy/server.pem")
	err = ws.ensurePushed(ctx)
	if err == nil {
		t.Fatal("ensurePushed shipped credential-looking files")
	}
	for _, name := range []string{".env", "deploy/server.pem"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
	if strings.Contains(err.Error(), "work.txt") {
		t.Errorf("error %q names an ordinary file", err)
	}
	if _, ok := ws.remoteBranchSHA(ctx); ok {
		t.Error("the branch reached the remote despite the refusal")
	}

	// With the files gone the same tree ships.
	for _, name := range []string{".env", "deploy/server.pem"} {
		if err := os.Remove(filepath.Join(ws.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.ensurePushed(ctx); err != nil {
		t.Fatalf("ensurePushed after the files were removed: %v", err)
	}
}
