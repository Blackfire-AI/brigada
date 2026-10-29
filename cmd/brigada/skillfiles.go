package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// Skill and role files (§5.9, §5.10) may carry a version header written by
// brigada: an HTML comment, `<!-- brigada: version=… digest=… -->`. A role
// file holds it as its first line; a SKILL.md keeps its frontmatter first
// and holds it right after the closing `---`. The header is no part of the
// text: every digest, and the text the engine stores, is over the whole
// file except the header, so an edit to any other byte shows.

// headerRe matches a header at the start of its input, with its line end.
var headerRe = regexp.MustCompile(`^<!--[ \t]*brigada:[^\n]*?-->[ \t]*(\r?\n)?`)

// maxHeaderBytes is the room a header may take on top of a text's bound.
const maxHeaderBytes = 512

// maxSkillFileBytes bounds a file the client reads under a skill directory
// before the engine's own bound applies.
const maxSkillFileBytes = 1 << 20

// textDigest is the SHA-256 the engine records for a text.
func textDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// stripRoleHeader drops a role file's first-line header.
func stripRoleHeader(text string) string {
	return headerRe.ReplaceAllString(text, "")
}

// splitSkillFile splits a SKILL.md into its frontmatter lines and the
// text without the header after the closing delimiter. The frontmatter
// stays part of the text.
func splitSkillFile(text string) (front []string, stripped string, err error) {
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		if rest, ok = strings.CutPrefix(text, "---\r\n"); !ok {
			return nil, "", errors.New("must start with a `---` frontmatter line")
		}
	}
	off := len(text) - len(rest)
	for len(rest) > 0 {
		line, tail, _ := strings.Cut(rest, "\n")
		consumed := len(rest) - len(tail)
		if strings.TrimRight(line, "\r") == "---" {
			end := off + consumed
			return front, text[:end] + headerRe.ReplaceAllString(text[end:], ""), nil
		}
		front = append(front, strings.TrimRight(line, "\r"))
		off += consumed
		rest = tail
	}
	return nil, "", errors.New("frontmatter is not closed by a `---` line")
}

// parseSkillFrontmatter reads name, description and the optional for; any
// other key is refused.
func parseSkillFrontmatter(lines []string) (name, desc, forRole string, err error) {
	seen := map[string]bool{}
	for _, l := range lines {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			return "", "", "", fmt.Errorf("frontmatter line %q: nested values are not supported", l)
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			return "", "", "", fmt.Errorf("frontmatter line %q: want `key: value`", l)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if seen[k] {
			return "", "", "", fmt.Errorf("frontmatter key %q appears twice", k)
		}
		seen[k] = true
		switch k {
		case "name":
			name = v
		case "description":
			desc = v
		case "for":
			forRole = v
		default:
			return "", "", "", fmt.Errorf("frontmatter key %q is not allowed; want name, description and optionally for", k)
		}
	}
	if name == "" {
		return "", "", "", errors.New("frontmatter has no name")
	}
	if desc == "" {
		return "", "", "", errors.New("frontmatter has no description")
	}
	switch forRole {
	case "", engine.SkillForWorker, engine.SkillForReviewer, engine.SkillForBoth:
	default:
		return "", "", "", fmt.Errorf("frontmatter for %q must be worker, reviewer or both", forRole)
	}
	return name, desc, forRole, nil
}

// resolveInRepo resolves rel under root through symlinks and requires the
// result to stay inside the repository. Refusals start with label.
func resolveInRepo(root, label, rel string) (realRoot, full string, err error) {
	realRoot, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("%s: resolve the repository root: %w", label, err)
	}
	full, err = filepath.EvalSymlinks(filepath.Join(realRoot, filepath.FromSlash(rel)))
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", label, err)
	}
	if inside, err := filepath.Rel(realRoot, full); err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("%s resolves outside the repository", label)
	}
	return realRoot, full, nil
}

// readSkills parses every skill under skillsDir (§5.10): each immediate
// subdirectory holding a SKILL.md is one skill, whose frontmatter name
// must equal the directory's. A missing directory holds no skills. Every
// refusal names the file. The digest is over the SKILL.md without its
// header; the tree digest over every regular file below the directory, by
// sorted path.
func readSkills(root, skillsDir string) ([]engine.Skill, error) {
	label := "skills_dir " + skillsDir
	_, dir, err := resolveInRepo(root, label, skillsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", label)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	var out []engine.Skill
	for _, ent := range entries {
		rel := skillsDir + "/" + ent.Name()
		_, sdir, err := resolveInRepo(root, rel, rel)
		if err != nil {
			if ent.IsDir() || ent.Type()&fs.ModeSymlink != 0 {
				return nil, err
			}
			continue
		}
		if st, err := os.Stat(sdir); err != nil || !st.IsDir() {
			continue
		}
		file := rel + "/SKILL.md"
		if _, err := os.Lstat(filepath.Join(sdir, "SKILL.md")); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		s, err := readSkill(root, ent.Name(), file, sdir)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// readSkill parses one skill's SKILL.md and digests its directory.
func readSkill(root, dirName, file, sdir string) (engine.Skill, error) {
	_, full, err := resolveInRepo(root, file, file)
	if err != nil {
		return engine.Skill{}, err
	}
	st, err := os.Stat(full)
	if err != nil {
		return engine.Skill{}, fmt.Errorf("%s: %w", file, err)
	}
	if !st.Mode().IsRegular() {
		return engine.Skill{}, fmt.Errorf("%s is not a regular file", file)
	}
	if st.Size() > maxSkillFileBytes {
		return engine.Skill{}, fmt.Errorf("%s is %d bytes, over the %d byte limit; shorten it (nothing is truncated)", file, st.Size(), engine.MaxSkillBytes)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return engine.Skill{}, fmt.Errorf("%s: %w", file, err)
	}
	front, text, err := splitSkillFile(string(b))
	if err != nil {
		return engine.Skill{}, fmt.Errorf("%s: %w", file, err)
	}
	name, desc, forRole, err := parseSkillFrontmatter(front)
	if err != nil {
		return engine.Skill{}, fmt.Errorf("%s: %w", file, err)
	}
	if name != dirName {
		return engine.Skill{}, fmt.Errorf("%s: frontmatter name %q must equal the directory name %q", file, name, dirName)
	}
	if len(text) > engine.MaxSkillBytes {
		return engine.Skill{}, fmt.Errorf("%s: skill %s is %d bytes, over the %d byte limit; shorten it (nothing is truncated)", file, name, len(text), engine.MaxSkillBytes)
	}
	tree, err := skillTreeDigest(sdir)
	if err != nil {
		return engine.Skill{}, fmt.Errorf("%s: %w", file, err)
	}
	return engine.Skill{Name: name, Description: desc, For: forRole, Text: text, Digest: textDigest(text), TreeDigest: tree}, nil
}

// skillTreeDigest hashes the regular files below dir: sorted slash paths,
// each with its content, SKILL.md without its header. Symlinks are not
// followed.
func skillTreeDigest(dir string) (string, error) {
	var rels []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			rels = append(rels, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(rels)
	h := sha256.New()
	for _, rel := range rels {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		text := string(b)
		if rel == "SKILL.md" {
			if _, text, err = splitSkillFile(text); err != nil {
				return "", err
			}
		}
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", rel, len(text), text)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// currentFileDigests are the digests of the files cfg names as they are on
// disk now, for status to compare with what was imported. A file that
// cannot be read is left out: its state is unknown.
func currentFileDigests(root string, cfg engine.ProjectConfig) api.FileDigests {
	d := api.FileDigests{Roles: map[string]string{}, Skills: map[string]engine.SkillFileDigest{}}
	for role, rel := range cfg.RoleFiles {
		if text, err := readRoleFile(root, role, rel); err == nil {
			d.Roles[role] = textDigest(text)
		}
	}
	if cfg.SkillsDir != "" {
		if skills, err := readSkills(root, cfg.SkillsDir); err == nil {
			for _, s := range skills {
				d.Skills[s.Name] = engine.SkillFileDigest{Digest: s.Digest, Tree: s.TreeDigest}
			}
		}
	}
	return d
}
