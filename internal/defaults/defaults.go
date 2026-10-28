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
const Version = "1.0.0"

// Roles are the per-role default texts (design §5.9): foreman,
// plan_review, worker and reviewer, each at <role>.md.
var Roles = []string{"foreman", "plan_review", "worker", "reviewer"}

// Skills are the shipped default skill texts, each at
// skills/<name>/SKILL.md.
var Skills = []string{"bugfix", "cli-flag", "config-setting", "dependency", "docs-change", "refactor", "schema-migration", "surface-parity"}

// Texts holds the role texts and skill files at their package-relative paths.
//
//go:embed *.md skills/*/SKILL.md
var Texts embed.FS

// archive holds every shipped version's default texts, at
// versions/<version>/<the same paths as Texts>, so a three-way diff can
// rebuild the exact text a file was generated from (design §5.9).
//
//go:embed versions
var archive embed.FS

// Digest returns the lowercase SHA-256 digest of an instruction text.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// GeneratedProvenance is how an upgrade's seeded default is recorded:
// the text was generated from the shipped version.
func GeneratedProvenance(version string) string {
	return "generated from " + version + ", by upgrade"
}

// IsGeneratedProvenance reports whether a recorded source names a text
// an upgrade generated from the shipped defaults.
func IsGeneratedProvenance(source string) bool {
	return strings.HasPrefix(source, "generated from ")
}

// RoleText returns the live default text of one role.
func RoleText(role string) (string, error) {
	data, err := Texts.ReadFile(role + ".md")
	if err != nil {
		return "", fmt.Errorf("role %q has no default text", role)
	}
	return string(data), nil
}

// SkillText returns the live default SKILL.md of one skill.
func SkillText(skill string) (string, error) {
	data, err := Texts.ReadFile("skills/" + skill + "/SKILL.md")
	if err != nil {
		return "", fmt.Errorf("skill %q has no default text", skill)
	}
	return string(data), nil
}

// Versions lists the shipped versions of the defaults, oldest first.
func Versions() ([]string, error) {
	entries, err := fs.ReadDir(archive, "versions")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ArchivedText returns a shipped version's default text by version and
// package-relative path — a role's "<role>.md" or a skill's
// "skills/<name>/SKILL.md" — so the exact text a file was generated
// from can be rebuilt (design §5.9).
func ArchivedText(version, name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", fmt.Errorf("default text %q is not a valid path", name)
	}
	data, err := archive.ReadFile("versions/" + version + "/" + name)
	if err != nil {
		return "", fmt.Errorf("version %q holds no default text %q", version, name)
	}
	return string(data), nil
}

// ArchivedRoleText is ArchivedText for one role's text.
func ArchivedRoleText(version, role string) (string, error) {
	return ArchivedText(version, role+".md")
}

// ArchivedSkillText is ArchivedText for one skill's SKILL.md.
func ArchivedSkillText(version, skill string) (string, error) {
	return ArchivedText(version, "skills/"+skill+"/SKILL.md")
}
