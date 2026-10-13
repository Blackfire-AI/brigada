package api

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/history"
)

// uploadArtifact posts one artifact blob through the runner protocol
// and returns the answer body.
func (e *env) uploadArtifact(token string, jobID int64, leaseID, kind string, data []byte) (int, []byte) {
	e.t.Helper()
	body := fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"kind":%q,"content":%q}`,
		jobID, leaseID, kind, base64.StdEncoding.EncodeToString(data))
	code, _, data2 := e.do(http.MethodPost, "/v1/runner/artifacts", token, "", []byte(body))
	return code, data2
}

func TestArtifactUploadEndpoint(t *testing.T) {
	e := newEnv(t)
	e.register("r1")
	e.register("r2")
	e.submitWork()
	_, data := e.lease("r1", 0)
	grant := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data)
	g := grant.Job

	transcript := []byte("raw event line one\nraw event line two\n")
	code, data := e.uploadArtifact("r1", g.JobID, g.LeaseID, "transcript", transcript)
	if code != http.StatusOK {
		t.Fatalf("upload: %d %s", code, data)
	}
	first := decode[struct {
		Answer     string `json:"answer"`
		ArtifactID int64  `json:"artifact_id"`
		SHA256     string `json:"sha256"`
	}](t, data)
	if first.Answer != "ok" || first.ArtifactID == 0 || len(first.SHA256) != 64 {
		t.Fatalf("upload answered %+v, want ok with the row", first)
	}

	// The same content for the same job and kind is idempotent.
	code, data = e.uploadArtifact("r1", g.JobID, g.LeaseID, "transcript", transcript)
	if code != http.StatusOK {
		t.Fatalf("repeat upload: %d %s", code, data)
	}
	second := decode[struct {
		Answer     string `json:"answer"`
		ArtifactID int64  `json:"artifact_id"`
	}](t, data)
	if second.Answer != "already" || second.ArtifactID != first.ArtifactID {
		t.Errorf("repeat upload = %+v, want already with artifact %d", second, first.ArtifactID)
	}

	// Fenced like a complete: the wrong lease and the wrong runner are
	// lost, not errors.
	code, data = e.uploadArtifact("r1", g.JobID, "not-the-lease", "transcript", transcript)
	if code != http.StatusOK || decode[answerBody](t, data).Answer != "lost" {
		t.Errorf("upload with a wrong lease = %d %s, want lost", code, data)
	}
	code, data = e.uploadArtifact("r2", g.JobID, g.LeaseID, "transcript", transcript)
	if code != http.StatusOK || decode[answerBody](t, data).Answer != "lost" {
		t.Errorf("upload by a non-holder = %d %s, want lost", code, data)
	}

	// An unknown kind is the caller's mistake.
	code, data = e.uploadArtifact("r1", g.JobID, g.LeaseID, "mystery", transcript)
	if code != http.StatusBadRequest {
		t.Errorf("unknown kind = %d %s, want 400", code, data)
	}
	// Malformed base64 is too.
	code, _, data = e.do(http.MethodPost, "/v1/runner/artifacts", "r1", "",
		[]byte(fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"kind":"transcript","content":"!!!"}`, g.JobID, g.LeaseID)))
	if code != http.StatusBadRequest {
		t.Errorf("malformed base64 = %d %s, want 400", code, data)
	}

	// Only a runner may upload.
	code, _, _ = e.do(http.MethodPost, "/v1/runner/artifacts", "alice", "",
		[]byte(fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"kind":"transcript","content":""}`, g.JobID, g.LeaseID)))
	if code != http.StatusForbidden {
		t.Errorf("upload by a client token = %d, want 403", code)
	}
}

// TestCompleteCarriesNoArtifactPaths is the hole the upload endpoint
// closed: a completion used to name artifact paths of the caller's
// choosing, and the artifact read would open them — a runner could
// plant /etc/passwd as its transcript and have any client read it. The
// field is gone from the protocol now: a body still carrying it is
// ignored, no row is created, and the artifact endpoint refuses the id.
func TestCompleteCarriesNoArtifactPaths(t *testing.T) {
	e := newEnv(t)
	e.register("r1")
	e.submitWork()
	_, data := e.lease("r1", 0)
	g := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job

	complete := fmt.Sprintf(`{"job_id":%d,"lease_id":%q,"result":"{\"status\":\"done\"}",`+
		`"artifacts":[{"kind":"transcript","path":"/etc/passwd","size":4096,"sha256":"0000000000000000000000000000000000000000000000000000000000000000"}]}`,
		g.JobID, g.LeaseID)
	code, _, data := e.do(http.MethodPost, "/v1/runner/complete", "r1", "", []byte(complete))
	if code != http.StatusOK {
		t.Fatalf("complete: %d %s", code, data)
	}
	if got := decode[answerBody](t, data).Answer; got != "ok" {
		t.Fatalf("complete answered %q, want ok: the run's outcome is not held hostage to the field", got)
	}

	// No artifact row exists, so no id reads anything — not the planted
	// path, not anything else.
	var rows int
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT COUNT(*) FROM artifacts").Scan(&rows)
	}); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if rows != 0 {
		t.Errorf("the completion created %d artifact rows, want none", rows)
	}
	for _, id := range []string{"1", "2", "99"} {
		code, _, _ = e.do(http.MethodGet, "/v1/artifacts/"+id, "alice", "", nil)
		if code != http.StatusNotFound {
			t.Errorf("get artifact %s after the planted complete = %d, want 404", id, code)
		}
	}
}

func TestHistoryEndpointScopesTheForeman(t *testing.T) {
	e := newEnv(t)
	jobID := e.submitWork()

	// A client reads the whole timeline.
	code, _, data := e.do(http.MethodGet, "/v1/history", "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("history as a client: %d %s", code, data)
	}
	page := decode[history.Page](t, data)
	if len(page.Items) < 2 {
		t.Fatalf("history items = %d, want the submit's event and message", len(page.Items))
	}
	if page.Items[0].Kind != "job_created" || page.Items[0].JobID != jobID {
		t.Errorf("first item = %+v, want the created event", page.Items[0])
	}

	// A foreman token is walled into its own project (§12.1): asking
	// for another's is forbidden, and asking for none means its own.
	code, _, data = e.do(http.MethodGet, "/v1/history?project=2", "foreman1", "", nil)
	if code != http.StatusForbidden {
		t.Errorf("history for another project = %d %s, want 403", code, data)
	}
	code, _, data = e.do(http.MethodGet, "/v1/history?project=1", "foreman1", "", nil)
	if code != http.StatusOK {
		t.Errorf("history for the own project = %d %s, want 200", code, data)
	}
	code, _, data = e.do(http.MethodGet, "/v1/history", "foreman1", "", nil)
	if code != http.StatusOK {
		t.Fatalf("history without a project = %d %s, want 200 scoped to the token's own", code, data)
	}
	scoped := decode[history.Page](t, data)
	if len(scoped.Items) != len(page.Items) {
		t.Errorf("the foreman's unscoped history = %d items, want its project's %d", len(scoped.Items), len(page.Items))
	}
	for _, it := range scoped.Items {
		if it.ProjectID != 1 {
			t.Errorf("the foreman read item %+v of project %d", it, it.ProjectID)
		}
	}

	// A runner token has no business here.
	code, _, _ = e.do(http.MethodGet, "/v1/history", "r1", "", nil)
	if code != http.StatusForbidden {
		t.Errorf("history as a runner = %d, want 403", code)
	}

	// Pagination rides the cursor: one item per page, oldest first.
	var kinds []string
	cursor := ""
	for {
		path := "/v1/history?limit=1"
		if cursor != "" {
			path += "&since=" + cursor
		}
		code, _, data = e.do(http.MethodGet, path, "alice", "", nil)
		if code != http.StatusOK {
			t.Fatalf("history page %s: %d %s", path, code, data)
		}
		p := decode[history.Page](t, data)
		for _, it := range p.Items {
			kinds = append(kinds, it.Kind)
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if strings.Join(kinds, ",") != strings.Join([]string{"job_created", "task_spec"}, ",") {
		t.Errorf("the paged timeline = %v", kinds)
	}
}

func TestArtifactGetStreamsTheBlob(t *testing.T) {
	e := newEnv(t)
	e.register("r1")
	e.submitWork()
	_, data := e.lease("r1", 0)
	g := decode[struct {
		Job *jobGrant `json:"job"`
	}](t, data).Job

	transcript := []byte("the session's raw event lines\n")
	code, data := e.uploadArtifact("r1", g.JobID, g.LeaseID, "transcript", transcript)
	if code != http.StatusOK {
		t.Fatalf("upload: %d %s", code, data)
	}
	artID := decode[struct {
		ArtifactID int64 `json:"artifact_id"`
	}](t, data).ArtifactID

	code, header, data := e.do(http.MethodGet, fmt.Sprintf("/v1/artifacts/%d", artID), "alice", "", nil)
	if code != http.StatusOK {
		t.Fatalf("get artifact: %d %s", code, data)
	}
	if string(data) != string(transcript) {
		t.Errorf("the streamed blob = %q, want the uploaded bytes", data)
	}
	if got := header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("content type = %q", got)
	}

	// The foreman of the artifact's project reads it; another answer is
	// forbidden, and a missing artifact is not found.
	code, _, _ = e.do(http.MethodGet, fmt.Sprintf("/v1/artifacts/%d", artID), "foreman1", "", nil)
	if code != http.StatusOK {
		t.Errorf("get artifact as the project's foreman = %d, want 200", code)
	}
	code, _, _ = e.do(http.MethodGet, "/v1/artifacts/999", "alice", "", nil)
	if code != http.StatusNotFound {
		t.Errorf("get a missing artifact = %d, want 404", code)
	}
	code, _, _ = e.do(http.MethodGet, fmt.Sprintf("/v1/artifacts/%d", artID), "r1", "", nil)
	if code != http.StatusForbidden {
		t.Errorf("get artifact as a runner = %d, want 403", code)
	}
}
