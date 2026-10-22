package runner

import (
	"path"
	"strconv"
	"strings"
)

// maxListedPaths bounds how many refused paths an error names.
const maxListedPaths = 10

// suspectLeftovers reads `git status --porcelain=v1 -z` output and
// returns the paths of files the worker added — untracked, or staged as
// new — that look like credentials or hidden configuration (§5.1). The
// runner stages whatever the worker left (commitLeftovers) and pushes
// it, so a stray .env or key would leave the machine with the branch.
// A file the repository already tracks is never listed: it is not new,
// and its changes are the worker's to make.
func suspectLeftovers(porcelain string) []string {
	var bad []string
	fields := strings.Split(porcelain, "\x00")
	for i := 0; i < len(fields); i++ {
		rec := fields[i]
		if len(rec) < 4 {
			continue
		}
		x, y, file := rec[0], rec[1], rec[3:]
		renamed := x == 'R' || x == 'C' || y == 'R' || y == 'C'
		if renamed {
			i++ // a rename or copy carries its source as the next field
		}
		// A rename or copy's path is its destination: a new name the
		// runner's git add -A commits, whatever file moved to it.
		if (x == '?' && y == '?') || x == 'A' || y == 'A' || renamed {
			if suspectPath(file) {
				bad = append(bad, file)
			}
		}
	}
	return bad
}

// templateSuffixes mark an env-like file as a committed template, not a
// holder of real values.
var templateSuffixes = []string{".example", ".sample", ".template", ".dist", ".tmpl", ".tpl"}

// envTemplate reports whether a lowercased env-like name is a template
// meant to be committed (.env.example), not a file of real values.
func envTemplate(name string) bool {
	for _, s := range templateSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// benignDotComponents are hidden names a repository commonly holds in
// the open: ignore files, editor and linter configuration, CI.
var benignDotComponents = map[string]bool{
	".github": true, ".gitlab": true, ".circleci": true, ".devcontainer": true,
	".husky": true, ".vscode": true, ".gitignore": true, ".gitattributes": true,
	".gitkeep": true, ".editorconfig": true, ".dockerignore": true, ".gitlab-ci.yml": true,
	".mailmap": true, ".nvmrc": true, ".node-version": true, ".go-version": true,
	".python-version": true, ".ruby-version": true, ".tool-versions": true,
	".npmignore": true, ".eslintignore": true, ".prettierignore": true, ".clang-format": true,
	".gitmodules": true,
}

// benignDotPrefixes cover the linter and formatter configurations that
// come in several spellings.
var benignDotPrefixes = []string{".golangci.", ".prettierrc", ".eslintrc", ".stylelintrc", ".babelrc"}

// suspectPath reports whether a new file at p looks like a credential or
// a hidden file the repository did not ask for: an env file, a key or
// certificate, a netrc, a credentials file, or any dotfile or dot
// directory outside the well-known open ones.
func suspectPath(p string) bool {
	parts := strings.Split(p, "/")
	base := strings.ToLower(parts[len(parts)-1])
	if envLike(base) {
		return true
	}
	for _, c := range parts {
		if !strings.HasPrefix(c, ".") || c == "." || c == ".." {
			continue
		}
		if lc := strings.ToLower(c); !benignDot(lc) && !(strings.HasPrefix(lc, ".env") && envTemplate(lc)) {
			return true
		}
	}
	return false
}

func benignDot(c string) bool {
	if benignDotComponents[c] {
		return true
	}
	for _, pre := range benignDotPrefixes {
		if strings.HasPrefix(c, pre) {
			return true
		}
	}
	return false
}

// envLike reports whether a lowercased file name is an environment file,
// a private key or certificate, or a credentials file.
func envLike(base string) bool {
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
		return !envTemplate(base)
	}
	switch path.Ext(base) {
	case ".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".kdbx", ".ppk":
		return true
	}
	for _, pre := range []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "credentials"} {
		if strings.HasPrefix(base, pre) {
			return true
		}
	}
	switch base {
	case ".netrc", "_netrc", ".npmrc", ".pypirc", ".pgpass", ".git-credentials":
		return true
	}
	return false
}

// listPaths joins up to limit paths, naming how many more there are.
func listPaths(paths []string, limit int) string {
	if len(paths) <= limit {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:limit], ", ") + ", and " + strconv.Itoa(len(paths)-limit) + " more"
}
