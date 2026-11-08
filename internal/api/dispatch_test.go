package api

import (
	"net/http"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestDispatchPauseRoles: pause and resume are the user's (§8.3, §12.1):
// a foreman credential is refused on both routes and changes nothing,
// while a client pauses and resumes the engine and one project.
func TestDispatchPauseRoles(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/v1/dispatch/pause", "/v1/dispatch/resume"} {
		code, _, data := e.do(http.MethodPost, path, "foreman1", "", []byte(`{}`))
		if code != http.StatusForbidden {
			t.Fatalf("foreman %s = %d %s, want 403", path, code, data)
		}
	}
	if origin, err := e.eng.LivePauseOrigin(e.ctx, 0); err != nil || origin != "" {
		t.Fatalf("the foreman's refused calls left a pause: %q, %v", origin, err)
	}

	// A client pauses the engine; the origin defaults to the user's.
	code, _, data := e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{}`))
	if code != http.StatusOK {
		t.Fatalf("client pause = %d %s", code, data)
	}
	if got := decode[PauseReply](t, data); !got.Started || got.Origin != engine.PauseOriginUser || got.ProjectID != 0 {
		t.Errorf("engine pause = %+v, want a started user pause of the engine", got)
	}

	// A project's pause by its repo URL, and its resume under the
	// engine's pause: lifted, and still paused by the engine.
	body := []byte(`{"repo_url":"https://example.invalid/one.git"}`)
	code, _, data = e.do(http.MethodPost, "/v1/dispatch/pause", "root", "", body)
	if code != http.StatusOK {
		t.Fatalf("admin project pause = %d %s", code, data)
	}
	if got := decode[PauseReply](t, data); got.ProjectID != e.project1 || got.RepoURL != "https://example.invalid/one.git" {
		t.Errorf("project pause = %+v", got)
	}
	code, _, data = e.do(http.MethodPost, "/v1/dispatch/resume", "alice", "", body)
	if code != http.StatusOK {
		t.Fatalf("client project resume = %d %s", code, data)
	}
	if got := decode[ResumeReply](t, data); !got.Resumed || got.StillPaused != "engine" {
		t.Errorf("project resume = %+v, want lifted and still paused by the engine", got)
	}

	// The drain-only lift leaves a user's pause alone, and names it.
	code, _, data = e.do(http.MethodPost, "/v1/dispatch/resume", "alice", "", []byte(`{"drain_only":true}`))
	if code != http.StatusOK {
		t.Fatalf("drain-only resume = %d %s", code, data)
	}
	if got := decode[ResumeReply](t, data); got.Resumed || got.StillPaused != engine.PauseOriginUser {
		t.Errorf("drain-only resume = %+v, want the user's pause kept", got)
	}

	// Refusals: an unknown project, a bad origin.
	if code, _, _ = e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{"repo_url":"https://example.invalid/none.git"}`)); code != http.StatusNotFound {
		t.Errorf("unknown project = %d, want 404", code)
	}
	if code, _, _ = e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{"origin":"x"}`)); code != http.StatusBadRequest {
		t.Errorf("bad origin = %d, want 400", code)
	}
}
