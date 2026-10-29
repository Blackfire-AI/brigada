package defaults

import (
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTexts(t *testing.T) {
	roles := map[string]bool{"foreman.md": false, "plan_review.md": false, "worker.md": false, "reviewer.md": false}
	skills := map[string]bool{"cli-flag": false, "config-setting": false, "bugfix": false, "surface-parity": false, "dependency": false, "schema-migration": false, "refactor": false, "docs-change": false}
	err := fs.WalkDir(Texts, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := Texts.ReadFile(name)
		if err != nil {
			return err
		}
		limit := 8192
		if strings.HasPrefix(name, "skills/") {
			limit = 4096
		}
		if len(data) == 0 || len(data) > limit || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
			t.Errorf("%s: invalid text or size %d (limit %d)", name, len(data), limit)
		}
		if limit == 8192 {
			if _, ok := roles[name]; !ok {
				t.Errorf("unexpected role %s", name)
			} else {
				roles[name] = true
			}
			return nil
		}
		dir := path.Base(path.Dir(name))
		if _, ok := skills[dir]; !ok || name != "skills/"+dir+"/SKILL.md" {
			t.Errorf("unexpected skill %s", name)
		} else {
			skills[dir] = true
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) < 5 || lines[0] != "---" || lines[1] != "name: "+dir || !strings.HasPrefix(lines[2], "description: ") || strings.TrimSpace(strings.TrimPrefix(lines[2], "description: ")) == "" || lines[3] != "---" {
			t.Errorf("%s: invalid frontmatter", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, found := range roles {
		if !found {
			t.Errorf("missing %s", name)
		}
	}
	for name, found := range skills {
		if !found {
			t.Errorf("missing skill %s", name)
		}
	}
}

func TestDigest(t *testing.T) {
	if got, want := Digest("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("Digest = %q, want %q", got, want)
	}
}

// TestArchiveHoldsEveryVersionWhole: every shipped version's directory
// carries exactly the twelve texts — four roles, eight skills — and
// nothing else, so a version is retrievable whole by version and name.
func TestArchiveHoldsEveryVersionWhole(t *testing.T) {
	versions, err := Versions()
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("the archive holds no version")
	}
	for _, ver := range versions {
		seen := map[string]int{}
		err := fs.WalkDir(Archive, "versions/"+ver, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			rel := strings.TrimPrefix(name, "versions/"+ver+"/")
			data, err := Archive.ReadFile(name)
			if err != nil {
				return err
			}
			if len(data) == 0 || !utf8.Valid(data) {
				t.Errorf("%s/%s: invalid text", ver, rel)
			}
			seen[rel]++
			return nil
		})
		if err != nil {
			t.Fatalf("walk version %s: %v", ver, err)
		}
		var want []string
		for _, r := range Roles {
			want = append(want, r+".md")
		}
		for _, s := range Skills {
			want = append(want, "skills/"+s+"/SKILL.md")
		}
		for _, w := range want {
			if seen[w] != 1 {
				t.Errorf("version %s: %s appears %d times, want exactly one", ver, w, seen[w])
			}
			delete(seen, w)
		}
		for extra := range seen {
			t.Errorf("version %s holds unexpected %s", ver, extra)
		}
	}
}

// TestCurrentArchiveEqualsLive: the current version's archive must equal
// the live embed, role texts and skills both, so the text a file was
// generated from is rebuilt exactly. A live text that changes must ship
// with a new version's directory and a bumped Version.
func TestCurrentArchiveEqualsLive(t *testing.T) {
	for _, r := range Roles {
		live, err := LiveRoleText(r)
		if err != nil {
			t.Fatalf("live role %s: %v", r, err)
		}
		arch, err := ArchiveRoleText(Version, r)
		if err != nil {
			t.Fatalf("archive role %s: %v", r, err)
		}
		if live != arch {
			t.Errorf("role %s: the %s archive drifted from the live text\n--- live:\n%s\n--- %s:\n%s", r, Version, live, Version, arch)
		}
	}
	for _, s := range Skills {
		live, err := LiveSkillText(s)
		if err != nil {
			t.Fatalf("live skill %s: %v", s, err)
		}
		arch, err := ArchiveSkillText(Version, s)
		if err != nil {
			t.Fatalf("archive skill %s: %v", s, err)
		}
		if live != arch {
			t.Errorf("skill %s: the %s archive drifted from the live text\n--- live:\n%s\n--- %s:\n%s", s, Version, live, Version, arch)
		}
	}
}

// TestWorkerStopRule: the default worker text carries the stop rule of
// issue #129 — a spec that cites a docs section the checkout does not
// carry stops the worker with needs_help, naming the section and the ref
// and commit it read; the worker never guesses and never writes the
// section itself. The default foreman text carries the matching blocking
// rule: the foreman fixes the base or the spec, or asks the user, and
// never answers "continue without it".
func TestWorkerStopRule(t *testing.T) {
	worker, err := LiveRoleText("worker")
	if err != nil {
		t.Fatalf("live worker text: %v", err)
	}
	for _, phrase := range []string{
		"stop with needs_help",
		"name the missing section and the base ref and commit",
		"do not guess",
		"never write the section yourself",
	} {
		if !strings.Contains(worker, phrase) {
			t.Errorf("worker.md never says %q", phrase)
		}
	}
	foreman, err := LiveRoleText("foreman")
	if err != nil {
		t.Fatalf("live foreman text: %v", err)
	}
	for _, phrase := range []string{
		"is blocking",
		"the task's base or the spec, or ask the user",
		`"continue without it"`,
	} {
		if !strings.Contains(foreman, phrase) {
			t.Errorf("foreman.md never says %q", phrase)
		}
	}
}

// TestArchiveRefusals: an unknown version, an unknown role or skill, and
// a name outside the archive are refused, never guessed.
func TestArchiveRefusals(t *testing.T) {
	if _, err := ArchiveText("0.0.1", "worker.md"); err == nil {
		t.Errorf("an unknown version was accepted")
	}
	if _, err := ArchiveText(Version, "../defaults.go"); err == nil {
		t.Errorf("a path outside the archive was accepted")
	}
	if _, err := ArchiveText(Version, "/etc/passwd"); err == nil {
		t.Errorf("an absolute path was accepted")
	}
	if _, err := ArchiveRoleText(Version, "boss"); err == nil {
		t.Errorf("an unknown role was accepted")
	}
	if _, err := ArchiveSkillText(Version, "no-such-skill"); err == nil {
		t.Errorf("an unknown skill was accepted")
	}
	if _, err := LiveRoleText("boss"); err == nil {
		t.Errorf("LiveRoleText took an unknown role")
	}
	if _, err := LiveSkillText("no-such-skill"); err == nil {
		t.Errorf("LiveSkillText took an unknown skill")
	}
}

var numberRe = regexp.MustCompile(`^(\d+)\. `)

// skillPhrases names the key phrases each skill's procedure must carry,
// so a rewrite that loses an item fails the test by name.
var skillPhrases = map[string][]string{
	"bugfix":           {"fails on the current code", "pass", "after it", "the cause", "documented behavior"},
	"cli-flag":         {"Register the flag", "help text", "usage text", "synopsis", "success test", "usage-error test", "compatibility", "sibling surfaces"},
	"config-setting":   {"Parse", "Validate", "Default", "Precedence", "table row", "test row"},
	"dependency":       {"lockfile", "license", "notices", "vulnerabilities"},
	"docs-change":      {"present tense", "against the code", "commands", "cross-links"},
	"refactor":         {"no public API change", "untouched", "renamed names"},
	"schema-migration": {"next migration number", "never renumber", "old data", "where the state lives"},
	"surface-parity":   {"CLI", "API", "MCP", "UI", "why not"},
}

// TestSkillProcedures: every skill carries a concrete numbered procedure
// of at least four steps, numbered from one without gaps, and each step
// set covers its checklist items as the phrases above name them.
func TestSkillProcedures(t *testing.T) {
	for _, s := range Skills {
		text, err := LiveSkillText(s)
		if err != nil {
			t.Fatalf("skill %s: %v", s, err)
		}
		next := 1
		steps := 0
		for _, line := range strings.Split(text, "\n") {
			m := numberRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if got, _ := strconv.Atoi(m[1]); got != next {
				t.Errorf("%s: step %s out of order, want %d", s, m[1], next)
				next = got + 1
				continue
			}
			next++
			steps++
		}
		if steps < 4 {
			t.Errorf("%s: %d numbered steps, want at least 4", s, steps)
		}
		lower := strings.ToLower(text)
		for _, phrase := range skillPhrases[s] {
			if !strings.Contains(lower, strings.ToLower(phrase)) {
				t.Errorf("%s: the procedure never says %q", s, phrase)
			}
		}
	}
}
