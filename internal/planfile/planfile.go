// Package planfile is the plan file (§6.4): the mirror of a plan the
// engine keeps in the repo, as .brigada/plans/<plan-id>.yaml on a branch
// of the project's remote. The file holds the graph, the task states, the
// branches and the open questions — never transcripts. The store is
// authoritative; the file is a mirror that may lag, and it exists so a
// lost store can be rebuilt from the repo (`brigada recover`).
//
// The YAML is hand-rolled in one fixed layout, with no dependency: the
// shape is ours, the writer is the reader's inverse, and Parse refuses
// anything the writer would not have written (full-line comments and
// blank lines aside).
package planfile

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// Dir is where plan files live in the repo.
const Dir = ".brigada/plans"

// Path is the repo path of a plan's file.
func Path(planID int64) string {
	return fmt.Sprintf("%s/%d.yaml", Dir, planID)
}

// Plan is one plan as the file shows it (§6.1, §6.2).
type Plan struct {
	ID      int64
	Title   string
	Repo    string // the project's repo URL
	State   string // the plan's state word: active, done, …
	Created time.Time
	Updated time.Time
	Tasks   []Task
}

// Task is one task of a plan. Deps name tasks of the same plan, so only
// a named task can be depended on (§6.1); ID is the store's row id, kept
// so an unnamed task is still identifiable.
type Task struct {
	Tier   string // the foreman's tier; empty means standard
	Effort string
	// ReviewTier and ReviewEffort are the task's own tier and effort for its
	// reviews (§7, §8); empty = the engine default.
	ReviewTier   string
	ReviewEffort string
	// Caps are the task's own round-cap overrides (§6.1), keyed
	// fix_rounds, review_rounds and restart; nil = the engine defaults.
	Caps     map[string]int
	ID       int64
	Name     string // '' = unnamed
	Title    string
	Class    string
	Deps     []string
	Base     string // the dep whose branch the work starts from; '' = the default branch
	Continue string // the dep whose author's session the first work job resumes (§6.1); '' = fresh
	State    string
	Branch   string // the result branch (§6.4); '' until the task pushed one
	Rounds   int    // fix and review rounds spent (§7)
	Question string // the open question (§3), '' when there is none
	// ExtraRepos are the read-only repositories the task was given (§5.8),
	// as the task recorded them: the canonical identity, never a URL that
	// could carry a credential.
	ExtraRepos []ExtraRepo
}

// ExtraRepo is one extra repository of a task (§5.8): its name, canonical
// repository identity and ref (” = the default branch).
type ExtraRepo struct {
	Name string
	Repo string
	Ref  string
}

const header = `# Brigada plan file (§6.4): a mirror of the engine's store, which stays
# authoritative. The engine rewrites this file on every plan change; edits
# here are overwritten. It holds the graph, the task states, the branches
# and the open questions — never transcripts.
`

const taskOptionsComment = "# Optional tier (light, standard, heavy) and effort (low, medium, high, max)\n# describe the model needed and how hard it should think (§6.1);\n# review_tier and review_effort do the same for its reviews (§7, §8); caps\n# override the task's fix_rounds, review_rounds and restart budgets (§6.1).\n"

// Keep the default file byte-for-byte stable; describe optional keys when used.
const tasksComment = tasksGraphComment + taskOptionsComment

const tasksGraphComment = `# §6.1: the graph. A task waits (pending) until every task named in deps is
# done; its work starts from the branch of base, one of deps — the continued
# dep when base is empty and continue is set, else the default branch; merge a
# task after its base. A task with continue, one of deps, starts in the
# session of the dep's author.
# States are the engine's (§6.1), rounds the fix and review rounds
# spent (§7), question the worker's open question (§3), when it has one.
`

var wordRE = regexp.MustCompile(`^[a-z_]+$`)

// Render writes the plan in the fixed layout. States are written bare, so
// they must be lowercase words (the engine's are); anything else is an
// error rather than a file Parse would refuse.
func Render(p Plan) ([]byte, error) {
	if !wordRE.MatchString(p.State) {
		return nil, fmt.Errorf("plan %d: state %q is not a lowercase word", p.ID, p.State)
	}
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "plan: %d\n", p.ID)
	fmt.Fprintf(&b, "title: %s\n", strconv.Quote(p.Title))
	fmt.Fprintf(&b, "repo: %s\n", strconv.Quote(p.Repo))
	fmt.Fprintf(&b, "state: %s\n", p.State)
	fmt.Fprintf(&b, "created: %s\n", p.Created.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "updated: %s\n", p.Updated.UTC().Format(time.RFC3339Nano))
	comment := tasksGraphComment
	for _, t := range p.Tasks {
		if (t.Tier != "" && t.Tier != adapter.TierStandard) || t.Effort != "" || t.ReviewTier != "" || t.ReviewEffort != "" || len(t.Caps) > 0 {
			comment = tasksComment
			break
		}
	}
	b.WriteString(comment)
	if len(p.Tasks) == 0 {
		b.WriteString("tasks: []\n")
		return b.Bytes(), nil
	}
	b.WriteString("tasks:\n")
	for _, t := range p.Tasks {
		if !wordRE.MatchString(t.State) {
			return nil, fmt.Errorf("plan %d task %d: state %q is not a lowercase word", p.ID, t.ID, t.State)
		}
		fmt.Fprintf(&b, "  - id: %d\n", t.ID)
		fmt.Fprintf(&b, "    name: %s\n", strconv.Quote(t.Name))
		fmt.Fprintf(&b, "    title: %s\n", strconv.Quote(t.Title))
		fmt.Fprintf(&b, "    class: %s\n", strconv.Quote(t.Class))
		deps := make([]string, len(t.Deps))
		for i, d := range t.Deps {
			deps[i] = strconv.Quote(d)
		}
		fmt.Fprintf(&b, "    deps: [%s]\n", strings.Join(deps, ", "))
		fmt.Fprintf(&b, "    base: %s\n", strconv.Quote(t.Base))
		if t.Continue != "" {
			fmt.Fprintf(&b, "    continue: %s\n", strconv.Quote(t.Continue))
		}
		if err := adapter.ValidateTier(t.Tier); err != nil {
			return nil, fmt.Errorf("task %d: %w", t.ID, err)
		}
		if err := adapter.ValidateTaskEffort(t.Effort); err != nil {
			return nil, fmt.Errorf("task %d: %w", t.ID, err)
		}
		if t.Tier != "" && t.Tier != adapter.TierStandard {
			fmt.Fprintf(&b, "    tier: %s\n", t.Tier)
		}
		if t.Effort != "" {
			fmt.Fprintf(&b, "    effort: %s\n", t.Effort)
		}
		if err := adapter.ValidateTier(t.ReviewTier); err != nil {
			return nil, fmt.Errorf("task %d: review %w", t.ID, err)
		}
		if err := adapter.ValidateTaskEffort(t.ReviewEffort); err != nil {
			return nil, fmt.Errorf("task %d: review %w", t.ID, err)
		}
		if t.ReviewTier != "" {
			fmt.Fprintf(&b, "    review_tier: %s\n", t.ReviewTier)
		}
		if t.ReviewEffort != "" {
			fmt.Fprintf(&b, "    review_effort: %s\n", t.ReviewEffort)
		}
		if len(t.Caps) > 0 {
			line, err := formatCaps(t.Caps)
			if err != nil {
				return nil, fmt.Errorf("task %d: %w", t.ID, err)
			}
			fmt.Fprintf(&b, "    caps: %s\n", line)
		}
		fmt.Fprintf(&b, "    state: %s\n", t.State)
		fmt.Fprintf(&b, "    branch: %s\n", strconv.Quote(t.Branch))
		fmt.Fprintf(&b, "    rounds: %d\n", t.Rounds)
		if len(t.ExtraRepos) > 0 {
			// Each entry is [name, repo, ref], ref "" for the default branch.
			items := make([]string, len(t.ExtraRepos))
			for i, x := range t.ExtraRepos {
				items[i] = fmt.Sprintf("[%s, %s, %s]", strconv.Quote(x.Name), strconv.Quote(x.Repo), strconv.Quote(x.Ref))
			}
			fmt.Fprintf(&b, "    extra_repos: [%s]\n", strings.Join(items, ", "))
		}
		if t.Question != "" {
			fmt.Fprintf(&b, "    question: %s\n", strconv.Quote(t.Question))
		}
	}
	return b.Bytes(), nil
}

// Parse reads a plan file back: the inverse of Render, strict. Full-line
// comments and blank lines may stand anywhere; every other line must be
// the one the layout expects next, or Parse names the line and stops. It
// is what recover reads plan files with (Fetch, §6.4), and what the
// mirror's commit message reads the file it is about to replace with.
func Parse(data []byte) (Plan, error) {
	r := &reader{}
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		r.lines = append(r.lines, numbered{no: i + 1, text: line})
	}
	var p Plan
	var err error
	if p.ID, err = r.intField("", "plan"); err != nil {
		return Plan{}, err
	}
	if p.Title, err = r.quotedField("", "title"); err != nil {
		return Plan{}, err
	}
	if p.Repo, err = r.quotedField("", "repo"); err != nil {
		return Plan{}, err
	}
	if p.State, err = r.wordField("", "state"); err != nil {
		return Plan{}, err
	}
	if p.Created, err = r.timeField("created"); err != nil {
		return Plan{}, err
	}
	if p.Updated, err = r.timeField("updated"); err != nil {
		return Plan{}, err
	}
	rest, err := r.field("", "tasks")
	if err != nil {
		return Plan{}, err
	}
	switch rest {
	case "[]":
	case "":
		for r.peekIs("  - id: ") {
			t, err := r.task()
			if err != nil {
				return Plan{}, err
			}
			p.Tasks = append(p.Tasks, t)
		}
	default:
		return Plan{}, r.errf("tasks: want [] or a list below, got %q", rest)
	}
	if line, ok := r.peek(); ok {
		return Plan{}, fmt.Errorf("line %d: unexpected %q", line.no, strings.TrimSpace(line.text))
	}
	return p, nil
}

type numbered struct {
	no   int
	text string
}

type reader struct {
	lines []numbered
	pos   int
	last  int // the number of the line last read, for end-of-file errors
}

func (r *reader) peek() (numbered, bool) {
	if r.pos >= len(r.lines) {
		return numbered{}, false
	}
	return r.lines[r.pos], true
}

func (r *reader) peekIs(prefix string) bool {
	l, ok := r.peek()
	return ok && strings.HasPrefix(l.text, prefix)
}

func (r *reader) errf(format string, args ...any) error {
	return fmt.Errorf("line %d: %s", r.last, fmt.Sprintf(format, args...))
}

// field reads the next line, which must be `<indent><key>: <rest>` (or
// `<indent><key>:` with nothing after), and returns rest.
func (r *reader) field(indent, key string) (string, error) {
	l, ok := r.peek()
	if !ok {
		return "", fmt.Errorf("line %d: missing %q", r.last+1, key)
	}
	r.pos++
	r.last = l.no
	want := indent + key + ":"
	switch {
	case l.text == want:
		return "", nil
	case strings.HasPrefix(l.text, want+" "):
		return strings.TrimPrefix(l.text, want+" "), nil
	}
	return "", fmt.Errorf("line %d: want %q, got %q", l.no, want, l.text)
}

func (r *reader) intField(indent, key string) (int64, error) {
	s, err := r.field(indent, key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != s {
		return 0, r.errf("%s: %q is not an integer", key, s)
	}
	return n, nil
}

func (r *reader) quotedField(indent, key string) (string, error) {
	s, err := r.field(indent, key)
	if err != nil {
		return "", err
	}
	v, err := unquote(s)
	if err != nil {
		return "", r.errf("%s: %v", key, err)
	}
	return v, nil
}

func (r *reader) wordField(indent, key string) (string, error) {
	s, err := r.field(indent, key)
	if err != nil {
		return "", err
	}
	if !wordRE.MatchString(s) {
		return "", r.errf("%s: %q is not a lowercase word", key, s)
	}
	return s, nil
}

func (r *reader) timeField(key string) (time.Time, error) {
	s, err := r.field("", key)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || !strings.HasSuffix(s, "Z") {
		return time.Time{}, r.errf("%s: %q is not a UTC RFC 3339 time", key, s)
	}
	return t, nil
}

// task reads one task block; the caller saw its "  - id: " line.
func (r *reader) task() (Task, error) {
	var t Task
	var err error
	if t.ID, err = r.intField("  - ", "id"); err != nil {
		return Task{}, err
	}
	const in = "    "
	if t.Name, err = r.quotedField(in, "name"); err != nil {
		return Task{}, err
	}
	if t.Title, err = r.quotedField(in, "title"); err != nil {
		return Task{}, err
	}
	if t.Class, err = r.quotedField(in, "class"); err != nil {
		return Task{}, err
	}
	deps, err := r.field(in, "deps")
	if err != nil {
		return Task{}, err
	}
	if t.Deps, err = parseList(deps); err != nil {
		return Task{}, r.errf("deps: %v", err)
	}
	// base is absent from files written before stacked bases.
	if r.peekIs(in + "base:") {
		if t.Base, err = r.quotedField(in, "base"); err != nil {
			return Task{}, err
		}
		if t.Base != "" && !slices.Contains(t.Deps, t.Base) {
			return Task{}, r.errf("base: %q is not one of deps", t.Base)
		}
	}
	// continue is absent from files written before continuing tasks, and
	// written only when the task continues a dep.
	if r.peekIs(in + "continue:") {
		if t.Continue, err = r.quotedField(in, "continue"); err != nil {
			return Task{}, err
		}
		if t.Continue == "" {
			return Task{}, r.errf("continue: written only when the task continues a dep")
		}
		if !slices.Contains(t.Deps, t.Continue) {
			return Task{}, r.errf("continue: %q is not one of deps", t.Continue)
		}
	}
	// The continued dep is the base (§6.1): a base naming another dep
	// is a file the engine would never write.
	if t.Base != "" && t.Continue != "" && t.Base != t.Continue {
		return Task{}, r.errf("base: %q but continue: %q: a continuing task's base is its continued dep", t.Base, t.Continue)
	}
	if r.peekIs(in + "tier:") {
		if t.Tier, err = r.wordField(in, "tier"); err != nil {
			return Task{}, err
		}
		if err := adapter.ValidateTier(t.Tier); err != nil {
			return Task{}, r.errf("%v", err)
		}
	}
	if r.peekIs(in + "effort:") {
		if t.Effort, err = r.wordField(in, "effort"); err != nil {
			return Task{}, err
		}
		if err := adapter.ValidateTaskEffort(t.Effort); err != nil {
			return Task{}, r.errf("%v", err)
		}
	}
	if r.peekIs(in + "review_tier:") {
		if t.ReviewTier, err = r.wordField(in, "review_tier"); err != nil {
			return Task{}, err
		}
		if err := adapter.ValidateTier(t.ReviewTier); err != nil {
			return Task{}, r.errf("review %v", err)
		}
	}
	if r.peekIs(in + "review_effort:") {
		if t.ReviewEffort, err = r.wordField(in, "review_effort"); err != nil {
			return Task{}, err
		}
		if err := adapter.ValidateTaskEffort(t.ReviewEffort); err != nil {
			return Task{}, r.errf("review %v", err)
		}
	}
	if r.peekIs(in + "caps:") {
		raw, err := r.field(in, "caps")
		if err != nil {
			return Task{}, err
		}
		if t.Caps, err = parseCaps(raw); err != nil {
			return Task{}, r.errf("caps: %v", err)
		}
	}
	if t.State, err = r.wordField(in, "state"); err != nil {
		return Task{}, err
	}
	if t.Branch, err = r.quotedField(in, "branch"); err != nil {
		return Task{}, err
	}
	rounds, err := r.intField(in, "rounds")
	if err != nil {
		return Task{}, err
	}
	if rounds < 0 || rounds > int64(^uint(0)>>1) {
		return Task{}, r.errf("rounds: %d out of range", rounds)
	}
	t.Rounds = int(rounds)
	// extra_repos is written only when the task has some (§5.8), so files
	// written before it stay valid.
	if r.peekIs(in + "extra_repos:") {
		raw, err := r.field(in, "extra_repos")
		if err != nil {
			return Task{}, err
		}
		if t.ExtraRepos, err = parseExtraRepos(raw); err != nil {
			return Task{}, r.errf("extra_repos: %v", err)
		}
	}
	if r.peekIs(in + "question:") {
		if t.Question, err = r.quotedField(in, "question"); err != nil {
			return Task{}, err
		}
		if t.Question == "" {
			return Task{}, r.errf("question: written only when there is one")
		}
	}
	return t, nil
}

// parseExtraRepos reads `[["name", "repo", "ref"], ...]`, the inverse of
// Render's extra_repos line. An empty list is refused: the line is written
// only when there is an entry.
// capKeys are the keys of a task's caps, in the order the file writes them.
var capKeys = []string{"fix_rounds", "review_rounds", "restart"}

const maxCap = 10

// formatCaps writes caps as {fix_rounds: 3, restart: 4}, keys in capKeys
// order; an unknown key or an out-of-range value is an error rather than a
// file Parse would refuse.
func formatCaps(caps map[string]int) (string, error) {
	var parts []string
	for _, k := range capKeys {
		v, ok := caps[k]
		if !ok {
			continue
		}
		if v < 0 || v > maxCap {
			return "", fmt.Errorf("caps: %s is %d; want 0 to %d", k, v, maxCap)
		}
		parts = append(parts, fmt.Sprintf("%s: %d", k, v))
	}
	if len(parts) != len(caps) {
		return "", fmt.Errorf("caps: unknown key; want %s", strings.Join(capKeys, ", "))
	}
	return "{" + strings.Join(parts, ", ") + "}", nil
}

// parseCaps reads what formatCaps wrote.
func parseCaps(s string) (map[string]int, error) {
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") || len(s) < 3 {
		return nil, fmt.Errorf("want {key: n, ...}, got %q", s)
	}
	caps := map[string]int{}
	for _, part := range strings.Split(s[1:len(s)-1], ", ") {
		k, v, found := strings.Cut(part, ": ")
		n, err := strconv.Atoi(v)
		if !found || err != nil || !slices.Contains(capKeys, k) || n < 0 || n > maxCap {
			return nil, fmt.Errorf("bad entry %q", part)
		}
		if _, dup := caps[k]; dup {
			return nil, fmt.Errorf("%s given twice", k)
		}
		caps[k] = n
	}
	return caps, nil
}

func parseExtraRepos(s string) ([]ExtraRepo, error) {
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("%q is not a [list]", s)
	}
	s = s[1 : len(s)-1]
	var out []ExtraRepo
	for s != "" {
		if len(out) > 0 {
			if !strings.HasPrefix(s, ", ") {
				return nil, fmt.Errorf("want \", \" between entries, got %q", s)
			}
			s = s[2:]
		}
		if !strings.HasPrefix(s, "[") {
			return nil, fmt.Errorf("want an entry [name, repo, ref], got %q", s)
		}
		s = s[1:]
		var parts [3]string
		for i := range parts {
			if i > 0 {
				if !strings.HasPrefix(s, ", ") {
					return nil, fmt.Errorf("want \", \" between the parts of an entry, got %q", s)
				}
				s = s[2:]
			}
			prefix, err := strconv.QuotedPrefix(s)
			if err != nil || !strings.HasPrefix(s, `"`) {
				return nil, fmt.Errorf("want a double-quoted part, got %q", s)
			}
			if parts[i], err = strconv.Unquote(prefix); err != nil {
				return nil, err
			}
			s = s[len(prefix):]
		}
		if !strings.HasPrefix(s, "]") {
			return nil, fmt.Errorf("want ] closing an entry, got %q", s)
		}
		s = s[1:]
		if parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("an entry needs a name and a repo")
		}
		out = append(out, ExtraRepo{Name: parts[0], Repo: parts[1], Ref: parts[2]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("written only when there is one")
	}
	return out, nil
}

// unquote reads a whole double-quoted string.
func unquote(s string) (string, error) {
	prefix, err := strconv.QuotedPrefix(s)
	if err != nil || prefix != s || !strings.HasPrefix(s, `"`) {
		return "", fmt.Errorf("%q is not one double-quoted string", s)
	}
	return strconv.Unquote(s)
}

// parseList reads `[]` or `["a", "b"]`.
func parseList(s string) ([]string, error) {
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("%q is not a [list]", s)
	}
	s = s[1 : len(s)-1]
	var out []string
	for s != "" {
		if len(out) > 0 {
			if !strings.HasPrefix(s, ", ") {
				return nil, fmt.Errorf("want \", \" between items, got %q", s)
			}
			s = s[2:]
		}
		prefix, err := strconv.QuotedPrefix(s)
		if err != nil || !strings.HasPrefix(s, `"`) {
			return nil, fmt.Errorf("want a double-quoted item, got %q", s)
		}
		v, err := strconv.Unquote(prefix)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		s = s[len(prefix):]
	}
	return out, nil
}

// Describe says what changed from the file the remote holds to the one
// about to replace it, in a phrase for the commit message: "created",
// "state active -> done", "task build running -> reviewing". old is nil
// for a first push; a change in no state or branch (rounds, a question)
// reads "updated".
func Describe(old *Plan, p Plan) string {
	if old == nil {
		return fmt.Sprintf("created: %s, %d tasks", p.State, len(p.Tasks))
	}
	var parts []string
	if old.State != p.State {
		parts = append(parts, fmt.Sprintf("state %s -> %s", old.State, p.State))
	}
	prev := map[int64]Task{}
	for _, t := range old.Tasks {
		prev[t.ID] = t
	}
	for _, t := range p.Tasks {
		label := t.Name
		if label == "" {
			label = "#" + strconv.FormatInt(t.ID, 10)
		}
		o, had := prev[t.ID]
		switch {
		case !had:
			parts = append(parts, fmt.Sprintf("task %s added (%s)", label, t.State))
		case o.State != t.State:
			parts = append(parts, fmt.Sprintf("task %s %s -> %s", label, o.State, t.State))
		case o.Branch != t.Branch:
			parts = append(parts, fmt.Sprintf("task %s branch %s", label, t.Branch))
		}
	}
	if len(parts) == 0 {
		return "updated"
	}
	const most = 4
	if len(parts) > most {
		parts = append(parts[:most], fmt.Sprintf("and %d more", len(parts)-most))
	}
	return strings.Join(parts, "; ")
}
