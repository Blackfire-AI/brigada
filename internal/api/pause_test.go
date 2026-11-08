package api

import (
	"net/http"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestDispatchGroup: pause and resume are client and admin actions (§12.1)
// — a foreman credential is refused on both with a 403 and pauses
// nothing — and a client pauses and resumes the engine and one project by
// its repo URL, each lift touching its own scope alone.
func TestDispatchGroup(t *testing.T) {
	e := newEnv(t)
	const repo = "https://example.invalid/one.git"

	for _, path := range []string{"/v1/dispatch/pause", "/v1/dispatch/resume"} {
		code, _, data := e.do(http.MethodPost, path, "foreman1", "", []byte(`{"repo_url":"`+repo+`"}`))
		if code != http.StatusForbidden {
			t.Errorf("foreman %s = %d %s, want 403", path, code, data)
		}
		if got := decode[errBody](t, data); got.Code != codeForbidden {
			t.Errorf("foreman %s error code = %q, want %q", path, got.Code, codeForbidden)
		}
	}
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Pauses) != 0 {
		t.Fatalf("the refused foreman paused something: %+v", st.Pauses)
	}

	// A client pauses the engine and the project, in the user's name.
	code, _, data := e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{}`))
	if code != http.StatusOK {
		t.Fatalf("client pauses the engine: %d %s", code, data)
	}
	if got := decode[engine.PauseResult](t, data); got.ProjectID != 0 || !got.Started || got.Origin != engine.PauseOriginUser {
		t.Errorf("engine pause = %+v, want a started user pause of scope 0", got)
	}
	code, _, data = e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{"repo_url":"`+repo+`"}`))
	if code != http.StatusOK {
		t.Fatalf("client pauses the project: %d %s", code, data)
	}
	if got := decode[engine.PauseResult](t, data); got.ProjectID != e.project1 || !got.Started {
		t.Errorf("project pause = %+v, want a started pause of project %d", got, e.project1)
	}
	// A repeat starts nothing.
	_, _, data = e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{}`))
	if got := decode[engine.PauseResult](t, data); got.Started {
		t.Errorf("a second engine pause = %+v, want it to start nothing", got)
	}

	// A repo with no project is a 404, a bad origin a 400.
	if code, _, data = e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{"repo_url":"https://example.invalid/none.git"}`)); code != http.StatusNotFound {
		t.Errorf("pause of an unknown repo = %d %s, want 404", code, data)
	}
	if code, _, data = e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{"origin":"bogus"}`)); code != http.StatusBadRequest {
		t.Errorf("pause with a bad origin = %d %s, want 400", code, data)
	}

	// The project's resume lifts the project's pause and not the engine's.
	code, _, data = e.do(http.MethodPost, "/v1/dispatch/resume", "alice", "", []byte(`{"repo_url":"`+repo+`"}`))
	if code != http.StatusOK {
		t.Fatalf("client resumes the project: %d %s", code, data)
	}
	if got := decode[engine.ResumeResult](t, data); got.ProjectID != e.project1 || !got.Resumed {
		t.Errorf("project resume = %+v, want its pause lifted", got)
	}
	st, err = e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Pauses) != 1 || st.Pauses[0].ProjectID != 0 {
		t.Errorf("pauses after the project resume = %+v, want the engine's alone", st.Pauses)
	}

	// drain_only leaves a user's pause alone and takes no repo URL.
	_, _, data = e.do(http.MethodPost, "/v1/dispatch/resume", "alice", "", []byte(`{"drain_only":true}`))
	if got := decode[engine.ResumeResult](t, data); got.Resumed {
		t.Errorf("drain-only resume lifted a user's pause: %+v", got)
	}
	if code, _, data = e.do(http.MethodPost, "/v1/dispatch/resume", "alice", "", []byte(`{"drain_only":true,"repo_url":"`+repo+`"}`)); code != http.StatusBadRequest {
		t.Errorf("drain-only with a repo = %d %s, want 400", code, data)
	}

	// The engine's resume lifts it, and a resume of nothing says so.
	_, _, data = e.do(http.MethodPost, "/v1/dispatch/resume", "root", "", []byte(`{}`))
	if got := decode[engine.ResumeResult](t, data); !got.Resumed || got.Origin != engine.PauseOriginUser {
		t.Errorf("engine resume = %+v, want the user's pause lifted", got)
	}
	_, _, data = e.do(http.MethodPost, "/v1/dispatch/resume", "root", "", []byte(`{}`))
	if got := decode[engine.ResumeResult](t, data); got.Resumed {
		t.Errorf("resume of an unpaused engine = %+v, want resumed=false", got)
	}
}

// TestDispatchDrainOrigin: a pause sent with origin drain stands as a
// drain's, a user's pause over it makes it the user's, and the drain-only
// lift takes the first and leaves the second.
func TestDispatchDrainOrigin(t *testing.T) {
	e := newEnv(t)
	_, _, data := e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{"origin":"drain"}`))
	if got := decode[engine.PauseResult](t, data); !got.Started || got.Origin != engine.PauseOriginDrain {
		t.Fatalf("drain pause = %+v, want a started drain pause", got)
	}
	_, _, data = e.do(http.MethodPost, "/v1/dispatch/resume", "alice", "", []byte(`{"drain_only":true}`))
	if got := decode[engine.ResumeResult](t, data); !got.Resumed || got.Origin != engine.PauseOriginDrain {
		t.Errorf("drain-only lift = %+v, want the drain pause lifted", got)
	}

	e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{"origin":"drain"}`))
	_, _, data = e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{}`))
	if got := decode[engine.PauseResult](t, data); got.Started || got.Origin != engine.PauseOriginUser {
		t.Errorf("user pause over a drain = %+v, want the origin upgraded to user", got)
	}
	_, _, data = e.do(http.MethodPost, "/v1/dispatch/pause", "alice", "", []byte(`{"origin":"drain"}`))
	if got := decode[engine.PauseResult](t, data); got.Origin != engine.PauseOriginUser {
		t.Errorf("drain over a user pause = %+v, want the origin to stay user", got)
	}
	_, _, data = e.do(http.MethodPost, "/v1/dispatch/resume", "alice", "", []byte(`{"drain_only":true}`))
	if got := decode[engine.ResumeResult](t, data); got.Resumed {
		t.Errorf("drain-only lift over a user pause = %+v, want it left alone", got)
	}
}

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
