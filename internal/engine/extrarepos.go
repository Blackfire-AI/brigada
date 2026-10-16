package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Extra repositories of a task (§5.8): other repositories a task reads
// but never changes, checked out read-only beside its worktree. A task
// names them, or inherits the project's default list; the effective list
// is resolved once, when the task is created, and recorded with it.

// SettingExtraRepos is the project default list (§5.8, §13.1). Schema 1 of
// the shared file is flat, so an entry is one string, `name url [ref]`,
// the parts separated by white space (a URL and a git ref hold none).
const SettingExtraRepos = "extra_repos"

// The sources of a recorded list (§5.8 "Resolution").
const (
	ExtraSourceDefault = "project-default"
	ExtraSourceTask    = "task"
)

// ExtraRepo is one extra repository. A caller names Name, Repo and Ref;
// resolution fills Repo with the canonical identity (§13.1 item 1) and URL
// with the credential-free URL the runner fetches from, since the identity
// alone (host/path) is not one.
type ExtraRepo struct {
	// Name is [a-z][a-z0-9_-]*; it names the checkout directory and the
	// variable BRIGADA_REPO_<NAME>.
	Name string `json:"name"`
	Repo string `json:"repo"`
	URL  string `json:"url,omitempty"`
	// Ref is a branch, tag or commit; empty = the repository's default
	// branch.
	Ref string `json:"ref,omitempty"`
}

// ExtraRepoEnvPrefix is reserved for the variables that name the checkouts, so
// none can clash with a variable the runner sets itself (§5.8).
const ExtraRepoEnvPrefix = "BRIGADA_REPO_"

// EnvVar is the variable that names the checkout's absolute path to setup,
// the worker and the check: the name uppercased, '-' turned into '_'.
func (r ExtraRepo) EnvVar() string {
	return ExtraRepoEnvPrefix + strings.ToUpper(strings.ReplaceAll(r.Name, "-", "_"))
}

// IsCommit reports whether the entry's ref is a full commit SHA, the only
// reproducible ref (§5.8).
func (r ExtraRepo) IsCommit() bool { return commitRefRe.MatchString(r.Ref) }

// ExtraReposSpec is a task's effective list as the task records it: the
// entries, where the list came from and, for the project default, the hash
// of the imported file it came from, so a later change to the setting
// leaves a recorded task alone.
type ExtraReposSpec struct {
	Repos  []ExtraRepo `json:"repos"`
	Source string      `json:"source"`
	Hash   string      `json:"hash,omitempty"`
}

var (
	extraNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	commitRefRe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// validateExtraRef refuses a ref git would read as an option or that is not
// one word.
func validateExtraRef(ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("ref %q must not start with '-'", ref)
	}
	for _, r := range ref {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("ref %q must be one word", ref)
		}
	}
	return nil
}

// resolveExtraRepos validates and canonicalizes a list a caller named. A
// nil list stays nil (inherit); an empty one stays empty (none). Names are
// [a-z][a-z0-9_-]*, two names giving one variable (a-b, a_b) or one name
// twice are refused rather than dropped, and a URL gives its canonical
// identity and its credential-free fetch URL (a token in it is dropped); a
// bare host/path, which names no way to fetch, is refused. Errors never
// quote a URL.
func resolveExtraRepos(in []ExtraRepo) ([]ExtraRepo, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]ExtraRepo, 0, len(in))
	byVar := map[string]string{}
	for i, x := range in {
		if !extraNameRe.MatchString(x.Name) {
			return nil, fmt.Errorf("%w: extra_repos entry %d: name %q must match [a-z][a-z0-9_-]*", ErrInvalidSetting, i+1, x.Name)
		}
		ev := x.EnvVar()
		if prev, dup := byVar[ev]; dup {
			if prev == x.Name {
				return nil, fmt.Errorf("%w: extra_repos names %q twice", ErrInvalidSetting, x.Name)
			}
			return nil, fmt.Errorf("%w: extra_repos names %q and %q both give %s", ErrInvalidSetting, prev, x.Name, ev)
		}
		byVar[ev] = x.Name
		parts, err := parseRepo(x.Repo)
		if err != nil {
			return nil, fmt.Errorf("%w: extra_repos %q: %v", ErrInvalidSetting, x.Name, err)
		}
		if parts.Transport == "" {
			return nil, fmt.Errorf("%w: extra_repos %q: a bare host/path names an identity but no way to fetch it; give an https://, ssh:// or scp-like URL", ErrInvalidSetting, x.Name)
		}
		ref := strings.TrimSpace(x.Ref)
		if err := validateExtraRef(ref); err != nil {
			return nil, fmt.Errorf("%w: extra_repos %q: %v", ErrInvalidSetting, x.Name, err)
		}
		out = append(out, ExtraRepo{Name: x.Name, Repo: parts.Canon, URL: parts.Transport, Ref: ref})
	}
	return out, nil
}

// parseExtraRepoItem reads one `name url [ref]` item of the setting.
func parseExtraRepoItem(item string) (ExtraRepo, error) {
	f := strings.Fields(item)
	if len(f) < 2 || len(f) > 3 {
		return ExtraRepo{}, fmt.Errorf("%w: %s item %q must be `name url [ref]`", ErrInvalidSetting, SettingExtraRepos, item)
	}
	x := ExtraRepo{Name: f[0], Repo: f[1]}
	if len(f) == 3 {
		x.Ref = f[2]
	}
	return x, nil
}

// extraRepoText renders an entry in the item spelling the setting takes.
func extraRepoText(x ExtraRepo) string {
	url := x.URL
	if url == "" {
		url = x.Repo
	}
	if x.Ref == "" {
		return x.Name + " " + url
	}
	return x.Name + " " + url + " " + x.Ref
}

// extraDefault is the project's default list as it stands now.
type extraDefault struct {
	repos []ExtraRepo
	// hash is the hash of the imported file the list came from; empty for a
	// local override.
	hash string
}

// projectExtraReposTx reads the project's default list.
func projectExtraReposTx(tx *sql.Tx, project int64) (extraDefault, error) {
	var raw, rawProv string
	if err := tx.QueryRow("SELECT settings, settings_prov FROM projects WHERE id = ?", project).Scan(&raw, &rawProv); err != nil {
		return extraDefault{}, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	if raw == "" {
		return extraDefault{}, nil
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &present); err != nil {
		return extraDefault{}, fmt.Errorf("parse the settings of project %d: %w", project, err)
	}
	cur, ok := present[SettingExtraRepos]
	if !ok {
		return extraDefault{}, nil
	}
	var d extraDefault
	if err := json.Unmarshal(cur, &d.repos); err != nil {
		return extraDefault{}, fmt.Errorf("parse %s of project %d: %w", SettingExtraRepos, project, err)
	}
	prov, err := parseProv(rawProv)
	if err != nil {
		return extraDefault{}, fmt.Errorf("parse the provenance of project %d: %w", project, err)
	}
	if prov.isImported(SettingExtraRepos, cur) {
		d.hash = prov.Hash
	}
	return d, nil
}

// spec is the record a task made now would carry: given is the task's own
// resolved list, nil to inherit the default (§5.8 "How a plan names them").
// A given list, even an empty one, replaces the default entirely, so an
// entry with a default one's name is the task's. nil when the effective
// list is empty.
func (d extraDefault) spec(given []ExtraRepo) *ExtraReposSpec {
	if given == nil {
		if len(d.repos) == 0 {
			return nil
		}
		return &ExtraReposSpec{Repos: d.repos, Source: ExtraSourceDefault, Hash: d.hash}
	}
	if len(given) == 0 {
		return nil
	}
	return &ExtraReposSpec{Repos: given, Source: ExtraSourceTask}
}

// extraReposRecord renders a spec for the tasks.extra_repos column; "" for
// none.
func extraReposRecord(spec *ExtraReposSpec) string {
	if spec == nil || len(spec.Repos) == 0 {
		return ""
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseExtraReposRecord reads a tasks.extra_repos column; nil for none or
// for a value that does not parse.
func parseExtraReposRecord(s string) *ExtraReposSpec {
	if s == "" {
		return nil
	}
	var spec ExtraReposSpec
	if json.Unmarshal([]byte(s), &spec) != nil || len(spec.Repos) == 0 {
		return nil
	}
	return &spec
}

// taskExtraReposTx reads the list a task recorded at creation.
func taskExtraReposTx(tx *sql.Tx, taskID int64) ([]ExtraRepo, error) {
	var rec sql.NullString
	err := tx.QueryRow("SELECT extra_repos FROM tasks WHERE id = ?", taskID).Scan(&rec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the extra repositories of task %d: %w", taskID, err)
	}
	if spec := parseExtraReposRecord(rec.String); spec != nil {
		return spec.Repos, nil
	}
	return nil, nil
}

// resolveTaskExtras resolves the lists of a plan's tasks (creation or
// amendment): each task's own list validated and canonicalized, and the
// record each would carry. label names task i in a refusal.
func resolveTaskExtras(def extraDefault, tasks []PlanTask, label func(i int) string) ([]PlanTask, []string, error) {
	out := make([]PlanTask, len(tasks))
	copy(out, tasks)
	recs := make([]string, len(tasks))
	for i := range out {
		given, err := resolveExtraRepos(out[i].ExtraRepos)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", label(i), err)
		}
		out[i].ExtraRepos = given
		recs[i] = extraReposRecord(def.spec(given))
	}
	return out, recs, nil
}

// extraRepoDescription says what the plan shown for approval says of one
// entry: its name, repository and ref, and whether the ref is a commit.
func extraRepoDescription(x ExtraRepo) string {
	switch {
	case x.Ref == "":
		return fmt.Sprintf("%s (%s, default branch, not a commit)", x.Name, x.Repo)
	case x.IsCommit():
		return fmt.Sprintf("%s (%s, commit %s)", x.Name, x.Repo, x.Ref)
	}
	return fmt.Sprintf("%s (%s, ref %s, not a commit)", x.Name, x.Repo, x.Ref)
}
