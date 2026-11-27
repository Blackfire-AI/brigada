package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// giteaRepo is the repo URL the Gitea tests give the fixture project: an
// owner and a repository under a path, as a Gitea forge serves them.
const giteaRepo = "https://git.example.invalid/acme/widget.git"

// giteaForge is a fake Gitea API on a local test server: the requests it
// saw, and the answers each path gives.
type giteaForge struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	answers  map[string]func(w http.ResponseWriter, r *http.Request)
}

func newGiteaForge(t *testing.T) *giteaForge {
	t.Helper()
	f := &giteaForge{answers: map[string]func(http.ResponseWriter, *http.Request){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		answer := f.answers[r.URL.Path]
		f.mu.Unlock()
		if answer == nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"The target couldn't be found."}`)
			return
		}
		answer(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// json answers a path with status and body.
func (f *giteaForge) json(path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// seen is a copy of the requests the forge saw.
func (f *giteaForge) seen() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...)
}

// host is the forge's host[:port], the <host> of tracker.<host>.token.
func (f *giteaForge) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

// giteaEnv is an env whose project reads the forge as its gitea tracker,
// with token as the forge's resolved token ("" = none configured).
func giteaEnv(t *testing.T, f *giteaForge, token string, adjust func(*Config)) *env {
	t.Helper()
	e := newEnvCfg(t, func(c *Config) {
		c.TrackerHTTP = f.srv.Client()
		c.IssueFetch = func(context.Context, string, int, bool) (*Issue, error) {
			t.Error("a gitea tracker read through the gh fetch")
			return nil, errors.New("gh fetch")
		}
		if token != "" {
			c.TrackerToken = func(host string) (string, bool, error) {
				if host != f.host() {
					return "", false, nil
				}
				return token, true, nil
			}
		}
		if adjust != nil {
			adjust(c)
		}
	})
	setGitea(t, e, f.srv.URL)
	return e
}

// setGitea points the fixture project at a gitea tracker and the
// owner/repository giteaRepo names.
func setGitea(t *testing.T, e *env, trackerURL string) {
	t.Helper()
	if _, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{SettingTracker: TrackerGitea, SettingTrackerURL: trackerURL}); err != nil {
		t.Fatalf("set the gitea tracker: %v", err)
	}
	e.exec("UPDATE projects SET repo_url = ? WHERE id = ?", giteaRepo, e.project)
}

const giteaIssuePath = "/api/v1/repos/acme/widget/issues/7"

// TestGiteaIssueRead covers the Gitea fetch (§12.2): GET requests alone
// on the forge's API path for the project's own owner/repository, the
// token sent as `Authorization: token`, and the issue — state, labels,
// pull-request flag, comments — filled alike with the gh path's.
func TestGiteaIssueRead(t *testing.T) {
	f := newGiteaForge(t)
	f.json(giteaIssuePath, 200, `{"number":7,"title":"Fix the frobnicator","body":"it frobs","html_url":"http://forge/acme/widget/pulls/7",
		"state":"closed","user":{"login":"gopher"},"labels":[{"name":"bug"},{"name":"help wanted"}],"pull_request":{"merged":true}}`)
	f.json(giteaIssuePath+"/comments", 200, `[{"user":{"login":"reviewer"},"body":"seconded","created_at":"2026-10-12T12:00:00Z"},
		{"user":{"login":"gopher"},"body":"thanks","created_at":"2026-10-12T13:00:00Z"}]`)
	e := giteaEnv(t, f, "tok-123", nil)

	iss, err := e.eng.ReadIssue(e.ctx, e.project, 7, true)
	if err != nil {
		t.Fatalf("ReadIssue: %v", err)
	}
	if iss.Number != 7 || iss.Title != "Fix the frobnicator" || iss.Body != "it frobs" || iss.Author != "gopher" ||
		iss.URL != "http://forge/acme/widget/pulls/7" || !iss.PullRequest || iss.State != "closed" {
		t.Errorf("issue = %+v, want the forge's whole", iss)
	}
	if strings.Join(iss.Labels, "|") != "bug|help wanted" {
		t.Errorf("labels = %q, want bug and help wanted in order", iss.Labels)
	}
	if len(iss.Comments) != 2 || iss.Comments[0].Author != "reviewer" || iss.Comments[0].Body != "seconded" ||
		iss.Comments[0].CreatedAt != "2026-10-12T12:00:00Z" || iss.Comments[1].Body != "thanks" {
		t.Errorf("comments = %+v, want both, oldest first", iss.Comments)
	}
	reqs := f.seen()
	if len(reqs) != 2 {
		t.Fatalf("the forge saw %d requests, want the issue and its comments", len(reqs))
	}
	for i, want := range []string{giteaIssuePath, giteaIssuePath + "/comments"} {
		r := reqs[i]
		if r.Method != http.MethodGet || r.URL.Path != want {
			t.Errorf("request %d = %s %s, want GET %s", i, r.Method, r.URL.Path, want)
		}
		if got := r.Header.Get("Authorization"); got != "token tok-123" {
			t.Errorf("request %d Authorization = %q, want the resolved token", i, got)
		}
	}
}

// TestGiteaIssueReadPlain covers an issue — Gitea sends pull_request
// null — read without comments and with no token configured: one GET, no
// Authorization header.
func TestGiteaIssueReadPlain(t *testing.T) {
	f := newGiteaForge(t)
	f.json(giteaIssuePath, 200, `{"number":7,"title":"t","body":"","state":"open","user":{"login":"gopher"},"labels":[],"pull_request":null}`)
	e := giteaEnv(t, f, "", nil)
	iss, err := e.eng.ReadIssue(e.ctx, e.project, 7, false)
	if err != nil {
		t.Fatalf("ReadIssue: %v", err)
	}
	if iss.PullRequest || iss.State != "open" || len(iss.Labels) != 0 || len(iss.Comments) != 0 {
		t.Errorf("issue = %+v, want an open issue with no labels or comments", iss)
	}
	if reqs := f.seen(); len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "" {
		t.Errorf("requests = %d; want one GET with no Authorization header", len(reqs))
	}
}

// TestGiteaIssueReadFailures covers the reasons a Gitea read answers
// instead of an issue: 404 is not-found, 401 and 403 are a refused read
// naming the token key, and any other answer is ErrIssueRead with the
// forge's message, bounded.
func TestGiteaIssueReadFailures(t *testing.T) {
	f := newGiteaForge(t)
	e := giteaEnv(t, f, "tok-123", nil)
	read := func(n int) error {
		t.Helper()
		_, err := e.eng.ReadIssue(e.ctx, e.project, n, true)
		return err
	}
	if err := read(404); !errors.Is(err, ErrIssueNotFound) || errors.Is(err, ErrIssueRead) {
		t.Errorf("404: err = %v, want ErrIssueNotFound", err)
	}
	for _, status := range []int{401, 403} {
		f.json(fmt.Sprintf("/api/v1/repos/acme/widget/issues/%d", status), status, `{"message":"token is required"}`)
		err := read(status)
		if !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), fmt.Sprintf("refused the read (HTTP %d)", status)) ||
			!strings.Contains(err.Error(), "tracker."+f.host()+".token") {
			t.Errorf("%d: err = %v, want ErrIssueRead naming the refused read and the token key", status, err)
		}
	}
	f.json("/api/v1/repos/acme/widget/issues/500", 500, `{"message":"database is locked"}`)
	if err := read(500); !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "answered HTTP 500: database is locked") {
		t.Errorf("500: err = %v, want ErrIssueRead with the forge's message", err)
	}
	f.json("/api/v1/repos/acme/widget/issues/502", 502, strings.Repeat("x", 4*noteBound))
	if err := read(502); !errors.Is(err, ErrIssueRead) || len(err.Error()) > 2*noteBound {
		t.Errorf("502: err is %d bytes, want ErrIssueRead with the body bounded", len(err.Error()))
	}
	f.json("/api/v1/repos/acme/widget/issues/9", 200, `not json`)
	if err := read(9); !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "the forge's answer is not an issue") {
		t.Errorf("garbage: err = %v, want ErrIssueRead saying the answer is not an issue", err)
	}
	// A comments failure fails the read too.
	f.json("/api/v1/repos/acme/widget/issues/10", 200, `{"number":10,"title":"t"}`)
	f.json("/api/v1/repos/acme/widget/issues/10/comments", 500, `{"message":"boom"}`)
	if err := read(10); !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "boom") {
		t.Errorf("comments 500: err = %v, want ErrIssueRead", err)
	}
	// A forge that cannot be reached is a read failure, not the URL.
	f.srv.Close()
	if err := read(7); !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "could not be reached") || strings.Contains(err.Error(), "/api/v1") {
		t.Errorf("unreachable: err = %v, want ErrIssueRead without the request URL", err)
	}
}

// TestGiteaRepoParts covers the owner/repository the Gitea fetch reads
// from a project's repo URL: the last two path segments in every URL
// form, and a refusal for one that names fewer, or a local path.
func TestGiteaRepoParts(t *testing.T) {
	for _, c := range []struct{ url, owner, repo string }{
		{"https://git.example.invalid/acme/widget.git", "acme", "widget"},
		{"git@git.example.invalid:acme/widget.git", "acme", "widget"},
		{"ssh://git@git.example.invalid:2222/acme/widget", "acme", "widget"},
		{"https://git.example.invalid/forge/acme/widget", "acme", "widget"},
	} {
		owner, repo, err := giteaParts(c.url)
		if err != nil || owner != c.owner || repo != c.repo {
			t.Errorf("%s: %q/%q, %v; want %s/%s", c.url, owner, repo, err, c.owner, c.repo)
		}
	}
	for _, bad := range []string{"https://git.example.invalid/widget.git", "/srv/git/acme/widget", "file:///srv/acme/widget"} {
		if _, _, err := giteaParts(bad); !errors.Is(err, ErrIssueRead) {
			t.Errorf("%s: err = %v, want ErrIssueRead", bad, err)
		}
	}
}

// TestReadIssueDispatch covers the tracker seam (§12.2): none reads
// nothing, github and unset read through the gh fetch, and an unset
// tracker off GitHub is refused with the reason naming the setting while
// an explicit github one says the repository is not on GitHub.
func TestReadIssueDispatch(t *testing.T) {
	var fetched int
	e := newEnvCfg(t, func(c *Config) {
		c.IssueFetch = func(_ context.Context, repoURL string, number int, _ bool) (*Issue, error) {
			fetched++
			if _, _, err := githubParts(repoURL); err != nil {
				return nil, err
			}
			return &Issue{Number: number, State: "open", Labels: []string{"bug"}}, nil
		}
	})
	set := func(kind string) {
		t.Helper()
		if _, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{SettingTracker: kind}); err != nil {
			t.Fatalf("set tracker=%q: %v", kind, err)
		}
	}
	set(TrackerNone)
	_, err := e.eng.ReadIssue(e.ctx, e.project, 3, false)
	if !errors.Is(err, ErrIssueRead) || err.Error() != trackerRefusal(TrackerNone, false).Error() || fetched != 0 {
		t.Errorf("tracker none: err = %v after %d fetches, want the none refusal and no fetch", err, fetched)
	}
	set("")
	_, err = e.eng.ReadIssue(e.ctx, e.project, 3, false)
	if err == nil || err.Error() != trackerRefusal("", false).Error() {
		t.Errorf("unset off GitHub: err = %v, want the unset refusal", err)
	}
	set(TrackerGitHub)
	_, err = e.eng.ReadIssue(e.ctx, e.project, 3, false)
	if !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "not on GitHub") || strings.Contains(err.Error(), "sets no tracker") {
		t.Errorf("github off GitHub: err = %v, want the gh refusal", err)
	}
	e.exec("UPDATE projects SET repo_url = 'https://github.com/acme/widget' WHERE id = ?", e.project)
	for _, kind := range []string{TrackerGitHub, ""} {
		set(kind)
		iss, err := e.eng.ReadIssue(e.ctx, e.project, 3, false)
		if err != nil || iss.State != "open" || len(iss.Labels) != 1 {
			t.Errorf("tracker %q on GitHub: %+v, %v; want the gh fetch's issue", kind, iss, err)
		}
	}
}

// TestParseIssueGitHub covers the gh path's parse of GitHub's REST
// answer: state and label names, the pull-request flag from the
// pull_request object, and the comment list.
func TestParseIssueGitHub(t *testing.T) {
	iss, err := parseIssue([]byte(`{"number":12,"title":"T","body":"B","html_url":"https://github.com/a/b/issues/12","state":"open",
		"user":{"login":"octocat"},"labels":[{"id":1,"name":"bug","color":"f00"},{"id":2,"name":"ui"}]}`), "gh's answer")
	if err != nil {
		t.Fatal(err)
	}
	if iss.Number != 12 || iss.Author != "octocat" || iss.State != "open" || strings.Join(iss.Labels, ",") != "bug,ui" || iss.PullRequest {
		t.Errorf("issue = %+v, want an open issue labelled bug and ui", iss)
	}
	pr, err := parseIssue([]byte(`{"number":13,"state":"closed","pull_request":{"url":"https://api.github.com/x"}}`), "gh's answer")
	if err != nil || !pr.PullRequest || pr.State != "closed" {
		t.Errorf("pull request = %+v, %v; want a closed pull request", pr, err)
	}
	if _, err := parseIssue([]byte(`[]`), "gh's answer"); !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "gh's answer is not an issue") {
		t.Errorf("garbage: err = %v", err)
	}
	cs, err := parseIssueComments([]byte(`[{"user":{"login":"r"},"body":"x","created_at":"2026-10-12T12:00:00Z"}]`), "gh's answer")
	if err != nil || len(cs) != 1 || cs[0].Author != "r" || cs[0].Body != "x" {
		t.Errorf("comments = %+v, %v", cs, err)
	}
}

// TestTrackerTokenNeverLeaks proves the tracker token goes to the forge
// request alone: a known token string never shows in an error, the
// issue the read returns, the engine's log, a job's lease grant — the
// whole of what a job is handed — or the store's files.
func TestTrackerTokenNeverLeaks(t *testing.T) {
	const token = "sekrit-tracker-token-4242"
	f := newGiteaForge(t)
	// A forge that echoes the Authorization header in its answers, the
	// worst case for a reason passed through.
	echo := func(status int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":"you sent %s"}`, r.Header.Get("Authorization"))
		}
	}
	f.mu.Lock()
	f.answers["/api/v1/repos/acme/widget/issues/500"] = echo(500)
	f.answers["/api/v1/repos/acme/widget/issues/401"] = echo(401)
	f.mu.Unlock()
	f.json(giteaIssuePath, 200, `{"number":7,"title":"t","body":"b","state":"open","labels":[{"name":"bug"}]}`)
	f.json(giteaIssuePath+"/comments", 200, `[]`)

	var logMu sync.Mutex
	var logged strings.Builder
	dir := t.TempDir()
	e := newEnvAt(t, filepath.Join(dir, "store.db"), func(c *Config) {
		c.TrackerHTTP = f.srv.Client()
		c.TrackerToken = func(string) (string, bool, error) { return token, true, nil }
		c.Warnf = func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			fmt.Fprintf(&logged, format+"\n", args...)
		}
	})
	setGitea(t, e, f.srv.URL)

	var seen []string
	for _, n := range []int{500, 401, 404} {
		_, err := e.eng.ReadIssue(e.ctx, e.project, n, true)
		if err == nil {
			t.Fatalf("issue %d read, want a failure", n)
		}
		seen = append(seen, err.Error())
	}
	if !strings.Contains(seen[0], "you sent token [token]") {
		t.Errorf("500 reason = %q, want the echoed token scrubbed", seen[0])
	}
	iss, err := e.eng.ReadIssue(e.ctx, e.project, 7, true)
	if err != nil {
		t.Fatalf("ReadIssue: %v", err)
	}
	rendered, _ := json.Marshal(iss)
	seen = append(seen, string(rendered), fmt.Sprintf("%+v", iss))
	if reqs := f.seen(); reqs[len(reqs)-1].Header.Get("Authorization") != "token "+token {
		t.Fatal("the forge never saw the token: the test proves nothing")
	}

	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	grant, _ := json.Marshal(g)
	seen = append(seen, string(grant), fmt.Sprintf("%+v", *g))
	logMu.Lock()
	seen = append(seen, logged.String())
	logMu.Unlock()
	for _, s := range seen {
		if strings.Contains(s, token) {
			t.Errorf("the token leaked into %q", s)
		}
	}
	e.st.Close()
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), token) {
			t.Errorf("the token is in the store file %s", file.Name())
		}
	}
}
