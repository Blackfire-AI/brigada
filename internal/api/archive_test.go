package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// Exercise the real authenticated routes and client codec in process: these
// calls need no listener, and the same handler serves the client socket.
func TestArchiveProjectClientAndRoles(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, engine.Config{})
	tokenFile := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokenFile, []byte(tokenFileBody), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(ctx, st, eng, nil, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()
	client := func(secret string) *Client {
		return &Client{token: secret, http: &http.Client{Transport: holdTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w.Result(), nil
		})}}
	}
	repo := "https://example.invalid/removed.git"
	admin := client("root-secret-1")
	user := client("alice-secret-1")
	_, err = admin.SubmitTask(ctx, TaskSubmit{RepoURL: repo, BaseRef: "main", Spec: "work"})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus := func(err error, status int, message string) {
		t.Helper()
		var se *StatusError
		if !errors.As(err, &se) || se.Status != status || !strings.Contains(err.Error(), message) {
			t.Fatalf("error = %v, want HTTP %d with %q", err, status, message)
		}
	}
	for _, token := range []string{"runner-1-secret", "foreman-secret-1"} {
		_, err := client(token).ArchiveProject(ctx, repo, true)
		wantStatus(err, http.StatusForbidden, "may not call")
	}
	_, err = user.ArchiveProject(ctx, repo, false)
	wantStatus(err, http.StatusConflict, "use cancel")
	if answer, err := user.ArchiveProject(ctx, repo, true); err != nil || answer != engine.AnsOK {
		t.Fatalf("archive: %s %v", answer, err)
	}
	if projects, err := user.Projects(ctx); err != nil || len(projects) != 0 {
		t.Fatalf("list after archive: %+v %v", projects, err)
	}
	if answer, err := admin.ArchiveProject(ctx, repo, false); err != nil || answer != engine.AnsAlready {
		t.Fatalf("repeat archive as admin: %s %v", answer, err)
	}
	_, err = user.AskForeman(ctx, repo, "hello", "")
	wantStatus(err, http.StatusConflict, "was removed; run brigada init in it to bring it back")
	if _, err := user.SetProjectForeman(ctx, repo, "claude", "sonnet", ""); err != nil {
		t.Fatal(err)
	}
	if projects, err := user.Projects(ctx); err != nil || len(projects) != 1 {
		t.Fatalf("restored project list: %+v %v", projects, err)
	}
	_, err = user.ArchiveProject(ctx, "https://example.invalid/unknown.git", false)
	wantStatus(err, http.StatusNotFound, "no project")
	_, err = user.ArchiveProject(ctx, "", false)
	wantStatus(err, http.StatusBadRequest, "repo URL is required")
	_, err = user.ArchiveProject(ctx, "https://user:password@example.invalid/leak.git", false)
	wantStatus(err, http.StatusNotFound, "no project")
	if strings.Contains(err.Error(), "password") {
		t.Fatalf("credential leaked: %v", err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/projects/archive", nil)
	r.Header.Set("Authorization", "Bearer alice-secret-1")
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET archive: %d %s", w.Code, w.Body)
	}
}
