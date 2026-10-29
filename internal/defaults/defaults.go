// Package defaults contains the default instruction texts shipped with brigada.
package defaults

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// Version identifies the brigada version these defaults ship with.
const Version = "1.0.1"

// Texts holds the role texts and skill files at their package-relative paths.
//
//go:embed *.md skills/*/SKILL.md
var Texts embed.FS

// Archive holds every shipped version's default texts under
// versions/<ver>/..., at the same package-relative paths as Texts, so a
// three-way diff can rebuild the exact text a file was generated from:
// live text, the version it was generated from, the new version's text.
// A release that changes a live text adds the new version's directory
// and bumps Version; older directories never change.
//
//go:embed all:versions
var Archive embed.FS

// Roles are the roles the package carries a text for, in setting order;
// each role's text lives at <role>.md.
var Roles = []string{"foreman", "plan_review", "reviewer", "worker"}

// Skills are the task-shape skills the package carries, each at
// skills/<name>/SKILL.md.
var Skills = []string{"bugfix", "cli-flag", "config-setting", "dependency", "docs-change", "refactor", "schema-migration", "surface-parity"}

// Digest returns the lowercase SHA-256 digest of an instruction text.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// LiveRoleText returns the live embedded text of a role, like "worker".
func LiveRoleText(role string) (string, error) {
	if !has(Roles, role) {
		return "", fmt.Errorf("unknown role %q; valid roles: %s", role, strings.Join(Roles, ", "))
	}
	b, err := Texts.ReadFile(role + ".md")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// LiveSkillText returns the live embedded SKILL.md text of a skill.
func LiveSkillText(skill string) (string, error) {
	if !has(Skills, skill) {
		return "", fmt.Errorf("unknown skill %q; valid skills: %s", skill, strings.Join(Skills, ", "))
	}
	b, err := Texts.ReadFile("skills/" + skill + "/SKILL.md")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Versions lists the shipped versions the archive holds, oldest first.
func Versions() ([]string, error) {
	entries, err := fs.ReadDir(Archive, "versions")
	if err != nil {
		return nil, err
	}
	if overlay != nil {
		if more, err := fs.ReadDir(overlay, "versions"); err == nil {
			entries = append(entries, more...)
		}
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !has(out, e.Name()) {
			out = append(out, e.Name())
		}
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

// ArchiveText returns a shipped version's archived text by name: the
// package-relative path of the file, like "worker.md" or
// "skills/cli-flag/SKILL.md". An unknown version or a name outside the
// archive is refused; nothing is guessed or substituted.
func ArchiveText(version, name string) (string, error) {
	versions, err := Versions()
	if err != nil {
		return "", err
	}
	if !has(versions, version) {
		return "", fmt.Errorf("unknown defaults version %q; the archive holds: %s", version, strings.Join(versions, ", "))
	}
	if !fs.ValidPath(name) {
		return "", fmt.Errorf("invalid archive path %q", name)
	}
	b, err := readArchive("versions/" + version + "/" + name)
	if err != nil {
		return "", fmt.Errorf("version %s holds no %q", version, name)
	}
	return string(b), nil
}

// ArchiveRoleText returns a shipped version's archived text of a role.
func ArchiveRoleText(version, role string) (string, error) {
	if !has(Roles, role) {
		return "", fmt.Errorf("unknown role %q; valid roles: %s", role, strings.Join(Roles, ", "))
	}
	return ArchiveText(version, role+".md")
}

// ArchiveSkillText returns a shipped version's archived SKILL.md of a
// skill, so an older generated skill can be reconstructed for diffs.
func ArchiveSkillText(version, skill string) (string, error) {
	if !has(Skills, skill) {
		return "", fmt.Errorf("unknown skill %q; valid skills: %s", skill, strings.Join(Skills, ", "))
	}
	return ArchiveText(version, "skills/"+skill+"/SKILL.md")
}
