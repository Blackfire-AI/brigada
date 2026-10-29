// Package defaults contains the default instruction texts shipped with brigada.
package defaults

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
)

// Version identifies the brigada version these defaults ship with.
const Version = "1.0.0"

// Texts holds the role texts and skill files at their package-relative paths.
//
//go:embed *.md skills/*/SKILL.md
var Texts embed.FS

// Archive holds every shipped version's default texts under
// versions/<ver>/..., mirroring Texts' layout inside each version: the
// role texts and the skills' SKILL.md files a binary of that version
// embedded, so a three-way diff can rebuild the exact text a file was
// generated from (design §5.9).
//
//go:embed versions
var Archive embed.FS

// Roles are the per-role instruction texts the package ships, by the
// names the store and the engine use for them (design §5.9).
var Roles = []string{"foreman", "plan_review", "worker", "reviewer"}

// SkillNames are the default task-shape skills the package ships.
var SkillNames = []string{"cli-flag", "config-setting", "bugfix", "surface-parity", "dependency", "schema-migration", "refactor", "docs-change"}

// Digest returns the lowercase SHA-256 digest of an instruction text.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// versionWord, roleWord and skillWord bound what a caller may spell as
// a version, a role or a skill name: the texts and the archive are
// addressed by path, so none may carry a separator or stand for a
// parent.
var (
	versionWord = regexp.MustCompile(`^[0-9][0-9A-Za-z.-]*$`)
	roleWord    = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	skillWord   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// LiveRoleText reads the live embedded text of a role, by role name.
func LiveRoleText(role string) (string, error) {
	if !roleWord.MatchString(role) {
		return "", fmt.Errorf("role %q is not one of %v", role, Roles)
	}
	data, err := Texts.ReadFile(role + ".md")
	if err != nil {
		return "", fmt.Errorf("read the default text of role %s: %w", role, err)
	}
	return string(data), nil
}

// LiveSkillText reads the live embedded text of a skill, by skill name.
func LiveSkillText(skill string) (string, error) {
	if !skillWord.MatchString(skill) {
		return "", fmt.Errorf("skill %q is not a skill name", skill)
	}
	data, err := Texts.ReadFile("skills/" + skill + "/SKILL.md")
	if err != nil {
		return "", fmt.Errorf("read the default text of skill %s: %w", skill, err)
	}
	return string(data), nil
}

// Versions lists the archive's versions, oldest first.
func Versions() ([]string, error) {
	entries, err := fs.ReadDir(Archive, "versions")
	if err != nil {
		return nil, fmt.Errorf("list the archived default versions: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !versionWord.MatchString(e.Name()) {
			return nil, fmt.Errorf("archive holds version %q, whose name is not a version", e.Name())
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// overlay holds archive files a test adds on top of Archive; nil in
// production.
var overlay fs.FS

// SetArchiveOverlay makes the files of f, laid out like Archive
// (versions/<ver>/...), readable beside the embedded archive until the
// returned function runs. It exists so a test can ship a fixture older
// version; nothing in the product calls it.
func SetArchiveOverlay(f fs.FS) (restore func()) {
	prev := overlay
	overlay = f
	return func() { overlay = prev }
}

// readArchive reads an archive path, the overlay first.
func readArchive(path string) ([]byte, error) {
	if overlay != nil {
		if data, err := fs.ReadFile(overlay, path); err == nil {
			return data, nil
		}
	}
	return Archive.ReadFile(path)
}

// RoleText returns a version's archived role text, by role name.
func RoleText(version, role string) (string, error) {
	if !versionWord.MatchString(version) {
		return "", fmt.Errorf("version %q is not a version", version)
	}
	if !roleWord.MatchString(role) {
		return "", fmt.Errorf("role %q is not one of %v", role, Roles)
	}
	data, err := readArchive("versions/" + version + "/" + role + ".md")
	if err != nil {
		return "", fmt.Errorf("read the archived %s text of version %s: %w", role, version, err)
	}
	return string(data), nil
}

// SkillText returns a version's archived skill text, by skill name.
func SkillText(version, skill string) (string, error) {
	if !versionWord.MatchString(version) {
		return "", fmt.Errorf("version %q is not a version", version)
	}
	if !skillWord.MatchString(skill) {
		return "", fmt.Errorf("skill %q is not a skill name", skill)
	}
	data, err := readArchive("versions/" + version + "/skills/" + skill + "/SKILL.md")
	if err != nil {
		return "", fmt.Errorf("read the archived skill %s of version %s: %w", skill, version, err)
	}
	return string(data), nil
}
