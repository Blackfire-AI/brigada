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
