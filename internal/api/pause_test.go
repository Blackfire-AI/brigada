package api

import (
	"net/http"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestStatusCarriesThePauses: the status endpoint carries each live
// pause (§8.3) — scope, origin, start, the jobs still draining and the
// queued jobs it holds — and the client's Status decodes them, the data
// the status renderings read.
func TestStatusCarriesThePauses(t *testing.T) {
	e := newEnv(t)
	job := e.submitWork()
	res, err := e.eng.Pause(e.ctx, e.project1, engine.PauseOriginUser, "cli")
	if err != nil {
		t.Fatalf("pause the project: %v", err)
	}
	code, _, data := e.do(http.MethodGet, "/v1/status", "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("status: %d %s", code, data)
	}
	st := decode[Status](t, data)
	if len(st.Pauses) != 1 {
		t.Fatalf("pauses = %+v, want the project's", st.Pauses)
	}
	p := st.Pauses[0]
	if p.ProjectID != e.project1 || p.RepoURL != "https://example.invalid/one.git" || p.Origin != engine.PauseOriginUser ||
		p.StartedAt != res.StartedAt || !p.Drained || len(p.Draining) != 0 || p.Queued != 1 {
		t.Errorf("the pause = %+v, want the project's user pause, drained, holding job %d", p, job)
	}
	for _, h := range st.Held {
		if h.ID == job {
			t.Errorf("held lists the paused job %d: %+v", job, h)
		}
	}
}

// TestPauseResumeDispatch: the dispatch group (§8.3, §12.1) is a client
// and admin action — a foreman credential is refused both verbs — and a
// client pauses and resumes each scope: the engine's with an empty
// repo URL, one project's by its URL.
func TestPauseResumeDispatch(t *testing.T) {
	e := newEnv(t)
	// A foreman credential cannot call either verb (§8.3).
	for _, path := range []string{"/v1/pause", "/v1/resume"} {
		code, _, data := e.do(http.MethodPost, path, "foreman1", "", []byte(`{}`))
		if code != http.StatusForbidden {
			t.Errorf("%s by the foreman: %d %s, want 403", path, code, data)
		}
	}
	// A client pauses the engine with no repo URL, and one project by
	// its repo URL; the answers name the scope and the origin.
	code, _, data := e.do(http.MethodPost, "/v1/pause", "alice", "", []byte(`{}`))
	if code != http.StatusOK {
		t.Fatalf("pause the engine: %d %s", code, data)
	}
	if res := decode[engine.PauseResult](t, data); !res.Started || res.ProjectID != 0 || res.Origin != engine.PauseOriginUser {
		t.Fatalf("the engine pause = %+v, want it started at scope 0 with origin user", res)
	}
	code, _, data = e.do(http.MethodPost, "/v1/pause", "alice", "", []byte(`{"repo_url":"https://example.invalid/one.git"}`))
	if code != http.StatusOK {
		t.Fatalf("pause the project: %d %s", code, data)
	}
	if res := decode[engine.PauseResult](t, data); !res.Started || res.ProjectID != e.project1 {
		t.Fatalf("the project pause = %+v, want it started at project %d", res, e.project1)
	}
	// A repo URL no project carries is the API's not-found.
	code, _, data = e.do(http.MethodPost, "/v1/pause", "alice", "", []byte(`{"repo_url":"https://example.invalid/none.git"}`))
	if code != http.StatusNotFound {
		t.Errorf("pause an unknown repo: %d %s, want 404", code, data)
	}
	// Resuming the project lifts its own pause alone: the engine's
	// stands. A client's success, and the refused foreman above it.
	code, _, data = e.do(http.MethodPost, "/v1/resume", "alice", "", []byte(`{"repo_url":"https://example.invalid/one.git"}`))
	if code != http.StatusOK {
		t.Fatalf("resume the project: %d %s", code, data)
	}
	if res := decode[engine.ResumeResult](t, data); !res.Resumed || res.ProjectID != e.project1 || res.Origin != engine.PauseOriginUser {
		t.Fatalf("the project resume = %+v, want the project's user pause lifted", res)
	}
	code, _, data = e.do(http.MethodPost, "/v1/resume", "alice", "", []byte(`{}`))
	if code != http.StatusOK {
		t.Fatalf("resume the engine: %d %s", code, data)
	}
	if res := decode[engine.ResumeResult](t, data); !res.Resumed || res.ProjectID != 0 {
		t.Fatalf("the engine resume = %+v, want the engine's pause lifted", res)
	}
	// The drain-only lift leaves a user's pause standing: an admin
	// pause of origin user, then the drain lift refuses it.
	if _, err := e.eng.Pause(e.ctx, 0, engine.PauseOriginUser, "root"); err != nil {
		t.Fatalf("pause the engine: %v", err)
	}
	code, _, data = e.do(http.MethodPost, "/v1/resume", "root", "", []byte(`{"drain_only":true}`))
	if code != http.StatusOK {
		t.Fatalf("the drain lift: %d %s", code, data)
	}
	if res := decode[engine.ResumeResult](t, data); res.Resumed {
		t.Fatalf("the drain lift = %+v, want the user's pause left standing", res)
	}
}
