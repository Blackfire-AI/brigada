package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// sentinelRepo is a password that starts with digits and a '/', which an
// identity-port shortcut would read as host:port/path. The secret in it
// must never reach an error, a log, a stored value or a status.
const (
	sentinelSecret = "s3cr3t-sentinel"
	sentinelRepo   = "user.name:1234/" + sentinelSecret + "/unsafe@example.invalid:team/repo"
)

func TestCanonicalize(t *testing.T) {
	const want = "example.invalid/team/repo"
	same := []string{
		"git@example.invalid:team/repo",
		"git@example.invalid:team/repo.git",
		"git@EXAMPLE.invalid:team/repo.git/",
		"example.invalid:team/repo",
		"ssh://git@example.invalid/team/repo.git",
		"ssh://git@example.invalid:22/team/repo",
		"https://example.invalid/team/repo.git",
		"https://example.invalid:443/team/repo",
		"HTTPS://Example.Invalid/team/repo/",
		"https://user:tok3n@example.invalid/team/repo.git",
		"https://tok3n@example.invalid/team/repo",
		"http://example.invalid:80/team/repo",
		"git://example.invalid/team/repo.git",
		"https://example.invalid//team///repo",
		"example.invalid/team/repo",
	}
	for _, raw := range same {
		got, err := Canonicalize(raw)
		if err != nil || got != want {
			t.Errorf("Canonicalize(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}

	cases := []struct{ raw, want string }{
		// Ports: default dropped, others kept, and a port is part of
		// the identity.
		{"ssh://git@example.invalid:2222/team/repo", "example.invalid:2222/team/repo"},
		{"https://example.invalid:8443/team/repo.git", "example.invalid:8443/team/repo"},
		{"https://example.invalid:80/team/repo", "example.invalid:80/team/repo"},
		{"ssh://example.invalid:443/team/repo", "example.invalid:443/team/repo"},
		{"ssh://localhost/team/repo", "localhost/team/repo"},
		// The canonical form itself is taken as it stands, so repeated
		// startups are stable even for a name ending .git.
		{"example.invalid/team/repo.git", "example.invalid/team/repo.git"},
		{"example.invalid:8443/team/repo", "example.invalid:8443/team/repo"},
		{"https://[::1]:8443/team/repo", "[::1]:8443/team/repo"},
		// .git, .git.git and slashes: exactly one .git goes.
		{"https://example.invalid/team/repo.git.git", "example.invalid/team/repo.git"},
		{"https://example.invalid/team/repo.git.git/", "example.invalid/team/repo.git"},
		{"https://example.invalid/team/repo.git///", "example.invalid/team/repo"},
		{"git@example.invalid:team/repo.git.git", "example.invalid/team/repo.git"},
		// Host case folds; path case stays.
		{"https://EXAMPLE.invalid/Team/Repo", "example.invalid/Team/Repo"},
		// file remotes and plain paths.
		{"file:///srv/git/repo.git", "file:/srv/git/repo.git"},
		{"file://localhost/srv/git/repo.git", "file:/srv/git/repo.git"},
		{"file:/srv/git/repo.git", "file:/srv/git/repo.git"},
		{"/srv/git//repo.git/", "file:/srv/git/repo.git"},
		{"/srv/git/./x/../repo.git", "file:/srv/git/repo.git"},
		{"file:///srv/a%20b/repo.git", "file:/srv/a b/repo.git"},
		{"/a b/c.git", "file:/a b/c.git"},
	}
	for _, tc := range cases {
		got, err := Canonicalize(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("Canonicalize(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}

	// Every canonical form is a fixed point: startup can run twice.
	for _, raw := range append(same, func() []string {
		var out []string
		for _, tc := range cases {
			out = append(out, tc.want)
		}
		return out
	}()...) {
		once, err := Canonicalize(raw)
		if err != nil {
			continue
		}
		twice, err := Canonicalize(once)
		if err != nil || twice != once {
			t.Errorf("Canonicalize(%q) = %q, then %q, %v; want a fixed point", raw, once, twice, err)
		}
	}
}

func TestCanonicalizeRefusesWhatTheStringCannotSettle(t *testing.T) {
	bad := []struct{ raw, problem string }{
		{"", "empty"},
		{"git@example.invalid:/abs/repo", "starting with '/'"},
		{"git@example.invalid:~me/repo", "starting with ~"},
		{"~/repo", "~"},
		{"~me/repo", "~"},
		{"gh:team/repo", "alias"},
		{"git@myalias:team/repo", "alias"},
		{"ssh://myalias/team/repo", "alias"},
		{"relative/dir", "ambiguous"},
		{"./repo", "ambiguous"},
		{"example.invalid", "ambiguous"},
		{"https://example.invalid", "no repository path"},
		{"https://example.invalid/", "no repository path"},
		{"https://example.invalid/team/../repo", "'..'"},
		{"https://example.invalid/team/repo?token=abc", "query"},
		{"ftp://example.invalid/team/repo", "scheme"},
		{"ssh://git@example.invalid:team/repo", "port"},
		{"https://example.invalid:99999/team/repo", "port"},
		{"https://exa mple.invalid/team/repo", "whitespace"},
		{"file://otherhost/srv/repo.git", "names a host"},
		{"file:relative/repo.git", "absolute"},
		{"/", "root"},
	}
	for _, tc := range bad {
		got, err := Canonicalize(tc.raw)
		if err == nil {
			t.Errorf("Canonicalize(%q) = %q, want an error", tc.raw, got)
			continue
		}
		if !errors.Is(err, ErrInvalidRepoURL) {
			t.Errorf("Canonicalize(%q) error = %v, want ErrInvalidRepoURL", tc.raw, err)
		}
		if !strings.Contains(err.Error(), tc.problem) {
			t.Errorf("Canonicalize(%q) error = %q, want it to name the problem (%q)", tc.raw, err, tc.problem)
		}
	}
}

// A password that starts with digits and a '/' must not slip past the
// credential check as a port: it is refused before any port is read, in
// each shape, and the secret is in no error.
func TestCanonicalizeRejectsADigitsSlashPassword(t *testing.T) {
	for _, raw := range []string{
		sentinelRepo,
		"https://user:1234/" + sentinelSecret + "/unsafe@example.invalid/team/repo",
		"ssh://user:1234/" + sentinelSecret + "@example.invalid/team/repo",
		"example.invalid:1234/" + sentinelSecret + "@evil.invalid:team/repo",
		"user:1234/" + sentinelSecret + "@example.invalid/team/repo",
		"git@example.invalid:team/" + sentinelSecret + "@x/repo",
	} {
		got, err := Canonicalize(raw)
		if err == nil {
			t.Errorf("Canonicalize(%q) = %q, want a refusal", RedactRepoURL(raw), got)
			continue
		}
		if strings.Contains(err.Error(), sentinelSecret) || strings.Contains(got, sentinelSecret) {
			t.Errorf("the refusal of %q leaks the secret: %q", RedactRepoURL(raw), err)
		}
		if shown := RedactRepoURL(raw); strings.Contains(shown, sentinelSecret) {
			t.Errorf("RedactRepoURL(%q) = %q, leaks the secret", raw, shown)
		}
	}
}

func TestRedactRepoURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://user:pw@example.invalid/team/repo.git":       "https://***@example.invalid/team/repo.git",
		"https://tok@example.invalid/repo?token=abc#frag":     "https://***@example.invalid/repo",
		"git@example.invalid:team/repo":                       "***@example.invalid:team/repo",
		"https://example.invalid/team/repo":                   "https://example.invalid/team/repo",
		"u:1234/" + sentinelSecret + "/x@example.invalid:t/r": "***@example.invalid:t/r",
	} {
		if got := RedactRepoURL(raw); got != want {
			t.Errorf("RedactRepoURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A project created with the https form is found by the scp form, the
// ssh:// form and the canonical string itself, and none makes a second.
func TestProjectLookupAcrossUrlForms(t *testing.T) {
	e := newEnv(t)
	set, err := e.eng.SetProjectForeman(e.ctx, "https://tok3n@example.invalid/team/lookup.git", "claude", "opus")
	if err != nil {
		t.Fatal(err)
	}
	if set.RepoURL != "example.invalid/team/lookup" {
		t.Errorf("the setting shows %q, want the canonical form", set.RepoURL)
	}
	for _, form := range []string{
		"git@example.invalid:team/lookup",
		"ssh://git@example.invalid/team/lookup.git",
		"https://example.invalid/team/lookup/",
		"example.invalid/team/lookup",
	} {
		got, err := e.eng.ProjectForeman(e.ctx, form)
		if err != nil || got == nil || got.ProjectID != set.ProjectID {
			t.Errorf("ProjectForeman(%q) = %+v, %v; want project %d", form, got, err, set.ProjectID)
		}
		if s, err := e.eng.ProjectSettings(e.ctx, form); err != nil || s == nil || s.ProjectID != set.ProjectID || s.RepoURL != set.RepoURL {
			t.Errorf("ProjectSettings(%q) = %+v, %v; want project %d", form, s, err, set.ProjectID)
		}
	}
	res := e.submitTask(TaskParams{RepoURL: "git@example.invalid:team/lookup.git", BaseRef: "main", Spec: "go"})
	if res.ProjectID != set.ProjectID {
		t.Errorf("a task by the scp form went to project %d, want %d", res.ProjectID, set.ProjectID)
	}
	if n := e.int("SELECT COUNT(*) FROM projects WHERE repo_url LIKE '%team/lookup%'"); n != 1 {
		t.Errorf("projects for the repo = %d, want 1", n)
	}
	if got := e.str("SELECT repo_url FROM projects WHERE id = ?", set.ProjectID); got != "example.invalid/team/lookup" {
		t.Errorf("stored repo_url = %q, want the canonical form", got)
	}
	if got := e.str("SELECT transport_url FROM projects WHERE id = ?", set.ProjectID); got != "https://example.invalid/team/lookup.git" {
		t.Errorf("stored transport = %q, want the https URL without its token", got)
	}
}

// insertLegacyProject writes a project row the way an older engine did:
// the URL as given in repo_url, no transport.
func (e *env) insertLegacyProject(repo string) int64 {
	e.t.Helper()
	e.exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES (?, 'main', '{}', 'claude', 'sonnet', ?, ?)`, repo, seedTS, seedTS)
	return e.int("SELECT id FROM projects WHERE repo_url = ?", repo)
}

func TestCanonicalizeProjectsRewritesOnceAndLeavesCollisionsApart(t *testing.T) {
	e := newEnv(t)
	plain := e.insertLegacyProject("https://example.invalid/team/plain.git")
	dotgit := e.insertLegacyProject("https://example.invalid/team/dotgit.git.git")
	file := e.insertLegacyProject("file:///srv/git/bare.git")
	dupA := e.insertLegacyProject("git@example.invalid:team/dup.git")
	dupB := e.insertLegacyProject("https://example.invalid/team/dup")
	bad := e.insertLegacyProject("myalias:team/odd")
	// A session and a task on a project to rewrite survive with their ids.
	e.exec("INSERT INTO sessions (key, state, created_at) VALUES (?, 'active', ?)",
		"foreman:"+itoa(plain), seedTS)
	seeded := e.int("SELECT COUNT(*) FROM tasks")

	rep, err := e.eng.CanonicalizeProjects(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The env's own seeded project plus plain, dotgit and file.
	if rep.Rewritten != 4 {
		t.Errorf("rewritten = %d, want 4 (the seeded project, plain, .git.git and file)", rep.Rewritten)
	}
	want := map[int64]string{
		e.project: "example.invalid/brigada",
		plain:     "example.invalid/team/plain",
		dotgit:    "example.invalid/team/dotgit.git",
		file:      "file:/srv/git/bare.git",
		// A collision group, and a row that does not canonicalize, stay
		// exactly as they were.
		dupA: "git@example.invalid:team/dup.git",
		dupB: "https://example.invalid/team/dup",
		bad:  "myalias:team/odd",
	}
	for id, repo := range want {
		if got := e.str("SELECT repo_url FROM projects WHERE id = ?", id); got != repo {
			t.Errorf("project %d repo_url = %q, want %q", id, got, repo)
		}
	}
	if got := e.str("SELECT transport_url FROM projects WHERE id = ?", file); got != "file:///srv/git/bare.git" {
		t.Errorf("the file project's transport = %q, want its file:// URL", got)
	}
	if got := e.str("SELECT transport_url FROM projects WHERE id = ?", dotgit); got != "https://example.invalid/team/dotgit.git.git" {
		t.Errorf("the .git.git project's transport = %q, want the URL as given", got)
	}
	for _, id := range []int64{dupA, dupB, bad} {
		if got := e.str("SELECT transport_url FROM projects WHERE id = ?", id); got != "" {
			t.Errorf("project %d got transport %q, want it untouched", id, got)
		}
	}
	if ids := rep.Collisions["example.invalid/team/dup"]; len(ids) != 2 || ids[0] != dupA || ids[1] != dupB {
		t.Errorf("collisions = %v, want projects %d and %d", rep.Collisions, dupA, dupB)
	}
	if _, ok := rep.Failed[bad]; !ok {
		t.Errorf("failed = %v, want project %d", rep.Failed, bad)
	}
	if got := e.int("SELECT COUNT(*) FROM tasks"); got != seeded {
		t.Errorf("tasks = %d, want the %d it had", got, seeded)
	}
	if got := e.int("SELECT COUNT(*) FROM sessions"); got != 1 {
		t.Errorf("sessions = %d, want 1", got)
	}
	if got := e.int("SELECT COUNT(*) FROM projects"); got != 7 {
		t.Errorf("projects = %d, want 7: nothing is merged", got)
	}

	// The duplicate shows in the status as something for the user.
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantItem := "projects " + itoa(dupA) + " and " + itoa(dupB) + " are the same repository (example.invalid/team/dup); "
	found := false
	for _, it := range info.NeedsYou {
		if strings.HasPrefix(it.Reason, wantItem) {
			found = true
		}
	}
	if !found {
		t.Errorf("needs you = %+v, want an item starting %q", info.NeedsYou, wantItem)
	}

	// A second run changes nothing.
	rep2, err := e.eng.CanonicalizeProjects(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Rewritten != 0 {
		t.Errorf("second run rewrote %d projects, want 0", rep2.Rewritten)
	}
	for id, repo := range want {
		if got := e.str("SELECT repo_url FROM projects WHERE id = ?", id); got != repo {
			t.Errorf("after the second run project %d repo_url = %q, want %q", id, got, repo)
		}
	}
}

// A collision group is detected before anything is rewritten, even when
// one member is already canonical and others would rewrite cleanly.
func TestCanonicalizeProjectsCollisionWithACanonicalRow(t *testing.T) {
	e := newEnv(t)
	canon := e.insertLegacyProject("example.invalid/team/both")
	raw := e.insertLegacyProject("ssh://git@example.invalid/team/both.git")
	other := e.insertLegacyProject("https://example.invalid/team/other.git")
	rep, err := e.eng.CanonicalizeProjects(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.str("SELECT repo_url FROM projects WHERE id = ?", raw); got != "ssh://git@example.invalid/team/both.git" {
		t.Errorf("the colliding raw row was rewritten to %q", got)
	}
	if got := e.str("SELECT repo_url FROM projects WHERE id = ?", canon); got != "example.invalid/team/both" {
		t.Errorf("the canonical row changed to %q", got)
	}
	if got := e.str("SELECT repo_url FROM projects WHERE id = ?", other); got != "example.invalid/team/other" {
		t.Errorf("the unrelated row = %q, want it rewritten", got)
	}
	if len(rep.Collisions) != 1 {
		t.Errorf("collisions = %v, want one group", rep.Collisions)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// The identity is no Git URL: a grant still carries a usable fetch URL
// for every transport, from the project or from the task's own.
func TestGrantsCarryAUsableTransportNotTheIdentity(t *testing.T) {
	cases := []struct{ submit, fetch string }{
		{"https://example.invalid/team/h.git", "https://example.invalid/team/h.git"},
		{"https://user:tok3n@example.invalid/team/h2.git", "https://example.invalid/team/h2.git"},
		{"git@example.invalid:team/s.git", "git@example.invalid:team/s.git"},
		{"ssh://git@example.invalid:2222/team/s2.git", "ssh://git@example.invalid:2222/team/s2.git"},
		{"file:///srv/git/f.git", "file:///srv/git/f.git"},
		{"/srv/git/p.git", "file:///srv/git/p.git"},
	}
	for _, tc := range cases {
		e := newEnv(t)
		res := e.submitTask(TaskParams{RepoURL: tc.submit, BaseRef: "main", Spec: "go"})
		g := e.lease(e.runner1, nil, nil)
		if g == nil || g.TaskID != res.TaskID {
			t.Fatalf("%s: lease = %+v, want the task's job", RedactRepoURL(tc.submit), g)
		}
		if g.RepoURL != tc.fetch {
			t.Errorf("%s: grant repo = %q, want the usable fetch URL %q", RedactRepoURL(tc.submit), g.RepoURL, tc.fetch)
		}
		if strings.Contains(g.RepoURL, "tok3n") {
			t.Errorf("%s: the grant carries the credential", RedactRepoURL(tc.submit))
		}
	}

	// A task that names another form of the project's repo fetches with
	// its own URL, as given (§5.1); one with none uses the project's.
	e := newEnv(t)
	first := e.submitTask(TaskParams{RepoURL: "https://example.invalid/team/two.git", BaseRef: "main", Spec: "one"})
	second := e.submitTask(TaskParams{RepoURL: "git@example.invalid:team/two.git", BaseRef: "main", Spec: "two"})
	if first.ProjectID != second.ProjectID {
		t.Fatalf("the two forms made projects %d and %d, want one", first.ProjectID, second.ProjectID)
	}
	got := map[int64]string{}
	for i := 0; i < 2; i++ {
		g := e.lease(e.runner1, nil, nil)
		if g == nil {
			t.Fatal("no grant")
		}
		got[g.TaskID] = g.RepoURL
		e.complete(g, done())
	}
	if got[first.TaskID] != "https://example.invalid/team/two.git" || got[second.TaskID] != "git@example.invalid:team/two.git" {
		t.Errorf("grants fetch from %v, want each task's own URL", got)
	}

	// Jobs with no task URL of their own, a standalone review and the
	// foreman's reading checkout, use the project's transport.
	e = newEnv(t)
	e.submitTask(TaskParams{RepoURL: "file:///srv/git/mine.git", BaseRef: "main", Spec: "x"})
	if transport := e.str("SELECT transport_url FROM projects WHERE repo_url LIKE 'file:%'"); transport != "file:///srv/git/mine.git" {
		t.Errorf("the file project's transport = %q", transport)
	}
	var repo string
	fileProject := e.int("SELECT id FROM projects WHERE repo_url LIKE 'file:%'")
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		var err error
		repo, _, err = standaloneReviewInfoTx(tx, &jobRow{ProjectID: fileProject})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if repo != "file:///srv/git/mine.git" {
		t.Errorf("a standalone review reads %q, want the project's file transport", repo)
	}
}

// Nothing the sentinel is part of reaches an error, a stored value, an
// event or the status, through any entry the engine has.
func TestSentinelCredentialNeverLeaks(t *testing.T) {
	e := newEnv(t)
	var errs []error
	_, err := e.eng.SubmitTask(e.ctx, TaskParams{RepoURL: sentinelRepo, BaseRef: "main", Spec: "go"})
	errs = append(errs, err)
	_, err = e.eng.SetProjectForeman(e.ctx, sentinelRepo, "claude", "opus")
	errs = append(errs, err)
	_, err = e.eng.SetProjectSettings(e.ctx, sentinelRepo, map[string]string{SettingApprovalMaxTasks: "3"})
	errs = append(errs, err)
	_, err = e.eng.ProjectForeman(e.ctx, sentinelRepo)
	errs = append(errs, err)
	_, err = e.eng.ProjectSettings(e.ctx, sentinelRepo)
	errs = append(errs, err)
	_, err = e.eng.AskForeman(e.ctx, sentinelRepo, "hello", "test")
	errs = append(errs, err)
	_, err = e.eng.ReviewBranch(e.ctx, sentinelRepo, "b")
	errs = append(errs, err)
	for i, err := range errs {
		if err == nil {
			t.Errorf("entry %d took the sentinel URL, want a refusal", i)
			continue
		}
		if strings.Contains(err.Error(), sentinelSecret) {
			t.Errorf("entry %d leaks the secret: %q", i, err)
		}
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM projects WHERE repo_url LIKE '%" + sentinelSecret + "%' OR transport_url LIKE '%" + sentinelSecret + "%'",
		"SELECT COUNT(*) FROM events WHERE payload LIKE '%" + sentinelSecret + "%'",
		"SELECT COUNT(*) FROM jobs WHERE prompt LIKE '%" + sentinelSecret + "%' OR COALESCE(result, '') LIKE '%" + sentinelSecret + "%'",
		"SELECT COUNT(*) FROM tasks WHERE COALESCE(repo_url, '') LIKE '%" + sentinelSecret + "%'",
	} {
		if n := e.int(q); n != 0 {
			t.Errorf("%s = %d, want the secret nowhere in the store", q, n)
		}
	}
	if n := e.int("SELECT COUNT(*) FROM projects"); n != 1 {
		t.Errorf("projects = %d, want only the seeded one: a refused URL creates nothing", n)
	}
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(info); strings.Contains(string(b), sentinelSecret) {
		t.Errorf("the status holds the secret: %s", b)
	}
}

// A legacy task-level repo override the canonical rules refuse is never
// handed to a runner, so no git error quotes it into a failure result or
// an event: the grant names no repo and the runner fails the job plainly.
func TestLegacyTaskOverrideWithACredentialIsNotGranted(t *testing.T) {
	e := newEnv(t)
	e.exec("UPDATE tasks SET repo_url = ? WHERE id = ?", sentinelRepo, e.task)
	e.submit(e.work())
	g := e.lease(e.runner1, nil, nil)
	if g == nil {
		t.Fatal("no grant")
	}
	if g.RepoURL != "" {
		t.Errorf("grant repo = %q, want none for an override the rules refuse", RedactRepoURL(g.RepoURL))
	}
	if b, _ := json.Marshal(g); strings.Contains(string(b), sentinelSecret) {
		t.Errorf("the grant holds the secret: %s", b)
	}
	e.complete(g, Completion{Result: `{"status":"failed","summary":"task 1: no repo URL"}`})
	for _, q := range []string{
		"SELECT COUNT(*) FROM events WHERE payload LIKE '%" + sentinelSecret + "%'",
		"SELECT COUNT(*) FROM jobs WHERE COALESCE(result, '') LIKE '%" + sentinelSecret + "%'",
	} {
		if n := e.int(q); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
}
