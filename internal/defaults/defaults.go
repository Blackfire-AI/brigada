// Package defaults contains the default instruction texts shipped with brigada.
package defaults

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
)

// Version identifies the brigada version these defaults ship with.
const Version = "1.0.0"

// Texts holds the role texts and skill files at their package-relative paths.
//
//go:embed *.md skills/*/SKILL.md
var Texts embed.FS

// Digest returns the lowercase SHA-256 digest of an instruction text.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
