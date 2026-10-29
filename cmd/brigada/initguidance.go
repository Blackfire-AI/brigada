package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/defaults"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// Init writes the full defaults (§5.9, §5.10): the four role files and the
// eight default skills, each under a marked header naming the brigada
// version and the digest of the generated text. The header is a comment
// the importer ignores (headerRe): a role file holds it as its first line,
// a SKILL.md right after its closing frontmatter line. A file is unedited
// while its text, header aside, still has the digest the header names.

// generatedHeader is the marked header for a generated text.
func generatedHeader(text string) string {
	return fmt.Sprintf("<!-- brigada: version=%s digest=%s -->\n", defaults.Version, defaults.Digest(text))
}

// skillFrontmatterEnd is the offset just past a SKILL.md's closing `---`
// line; false when the text has no frontmatter.
func skillFrontmatterEnd(text string) (int, bool) {
	if !strings.HasPrefix(text, "---\n") {
		return 0, false
	}
	i := strings.Index(text[3:], "\n---\n")
	if i < 0 {
		return 0, false
	}
	return 3 + i + len("\n---\n"), true
}

// generatedRoleFile and generatedSkillFile are what init writes for a
// default text.
func generatedRoleFile(text string) string { return generatedHeader(text) + text }

func generatedSkillFile(text string) string {
	end, ok := skillFrontmatterEnd(text)
	if !ok {
		return text
	}
	return text[:end] + generatedHeader(text) + text[end:]
}

var headerFieldsRe = regexp.MustCompile(`version=(\S+)\s+digest=([0-9a-f]+)`)

// splitGenerated separates a file into its header's version and digest and
// the text without the header. ok is false when the file carries no brigada
// header, so it is not a generated file.
func splitGenerated(file string, skill bool) (version, digest, text string, ok bool) {
	at := 0
	if skill {
		end, found := skillFrontmatterEnd(file)
		if !found {
			return "", "", file, false
		}
		at = end
	}
	h := headerRe.FindString(file[at:])
	if h == "" {
		return "", "", file, false
	}
	m := headerFieldsRe.FindStringSubmatch(h)
	text = file[:at] + file[at+len(h):]
	if m == nil {
		return "", "", text, false
	}
	return m[1], m[2], text, true
}

// isUnedited reports whether a file is a generated file nobody changed:
// its header is intact and the text digests to what the header names.
func isUnedited(file string, skill bool) bool {
	_, digest, text, ok := splitGenerated(file, skill)
	return ok && defaults.Digest(text) == digest
}

// guidanceTarget is one generated file: where it lives and its default text.
type guidanceTarget struct {
	rel   string
	text  string
	skill bool
	what  string
	// name is the role or skill name the commands take.
	name string
}

// guidanceTargets lists the files init generates for cfg: the role files
// the nested instructions map names, and the default skills under
// skills_dir. A project.yaml that uses the old instructions_file alias, or
// names no skills_dir, gets no files for them.
func guidanceTargets(cfg engine.ProjectConfig) ([]guidanceTarget, error) {
	var out []guidanceTarget
	for _, role := range engine.RoleKeys {
		rel, ok := cfg.RoleFiles[role]
		if !ok {
			continue
		}
		text, err := defaults.LiveRoleText(role)
		if err != nil {
			return nil, err
		}
		out = append(out, guidanceTarget{rel: rel, text: text, what: "default " + role + " instructions", name: role})
	}
	if cfg.SkillsDir != "" {
		for _, name := range defaults.Skills {
			text, err := defaults.LiveSkillText(name)
			if err != nil {
				return nil, err
			}
			out = append(out, guidanceTarget{rel: cfg.SkillsDir + "/" + name + "/SKILL.md", text: text, skill: true, what: "default skill " + name, name: name})
		}
	}
	return out, nil
}

// planGuidance plans the generated files (§5.9). A missing file is
// created; an unedited one is rewritten when the default moved on; an
// edited one is left as it is, with a note saying what version it was
// generated from and the diff to run. It reads and writes nothing else.
func planGuidance(root string, cfg engine.ProjectConfig) []wireFile {
	targets, err := guidanceTargets(cfg)
	if err != nil {
		return []wireFile{{rel: engine.ProjectConfigPath, err: err}}
	}
	var files []wireFile
	for _, t := range targets {
		t := t
		want := generatedRoleFile(t.text)
		if t.skill {
			want = generatedSkillFile(t.text)
		}
		f := planSteps(root, []wireStep{{t.rel, func(old []byte, exists bool) ([]byte, string, error) {
			if !exists {
				return []byte(want), "created", nil
			}
			version, digest, body, generated := splitGenerated(string(old), t.skill)
			switch {
			case generated && defaults.Digest(body) == digest:
				if digest == defaults.Digest(t.text) {
					return old, "unchanged", nil
				}
				return []byte(want), "replaced", nil
			case body == t.text || (!generated && string(old) == t.text) || (generated && digest == defaults.Digest(t.text)):
				// Nothing to do: it already is the default, or it is the
				// user's edit of a default that has not moved since.
				return old, "unchanged", nil
			}
			if !generated || version == "" {
				version = "unknown"
			}
			return old, "unchanged", editedNote{version: version, name: t.name}
		}}})[0]
		f.generated = true
		var note editedNote
		if errors.As(f.err, &note) {
			// An edited file is no error: it stays, and the plan says why.
			f.err = nil
			f.next = f.old
			f.state = "unchanged"
			f.purpose = fmt.Sprintf("edited since version %s, new default differs; kept. See: brigada instructions diff %s (brigada init --take-default %s resets it)", note.version, note.name, note.name)
		} else if f.purpose == "" {
			f.purpose = t.what
		}
		files = append(files, f)
	}
	return files
}

// editedNote carries an edited file's version through a wireStep.
type editedNote struct{ version, name string }

func (e editedNote) Error() string { return "edited since version " + e.version }

// removalSteps plans the removal of the generated files: only an unedited
// generated file is deleted, and an edited one stays (§5.9).
func guidanceRemovalSteps(root string) []wireStep {
	cfg, err := engine.ParseProjectConfig(readOrNil(filepath.Join(root, filepath.FromSlash(engine.ProjectConfigPath))))
	if err != nil {
		// No readable project.yaml: the default paths.
		cfg = engine.ProjectConfig{RoleFiles: map[string]string{}, SkillsDir: engine.DefaultSkillsDir}
		for _, role := range engine.RoleKeys {
			cfg.RoleFiles[role] = engine.DefaultRoleFile(role)
		}
	}
	targets, err := guidanceTargets(cfg)
	if err != nil {
		return nil
	}
	var steps []wireStep
	for _, t := range targets {
		skill := t.skill
		steps = append(steps, wireStep{t.rel, func(d []byte, exists bool) ([]byte, string, error) {
			if !exists {
				return d, "unchanged", nil
			}
			if !isUnedited(string(d), skill) {
				return nil, "", errors.New("edited since generated; keeping your file")
			}
			return nil, "removed", nil
		}})
	}
	return steps
}

func readOrNil(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}
