package projecturl

import "testing"

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"https://tk@example.invalid/org/repo.git":       "https://***@example.invalid/org/repo.git",
		"https://user:pw@example.invalid/org/repo.git":  "https://***@example.invalid/org/repo.git",
		"https://example.invalid/org/repo.git?k=v#frag": "https://example.invalid/org/repo.git",
		"git@example.invalid:org/repo.git":              "***@example.invalid:org/repo.git",
		"https://example.invalid/org/repo.git":          "https://example.invalid/org/repo.git",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactText(t *testing.T) {
	for in, want := range map[string]string{
		"fatal: unable to access 'https://tk@example.invalid/r.git/': 403":                          "fatal: unable to access 'https://***@example.invalid/r.git/': 403",
		"fatal: unable to access 'https://u:pw@example.invalid/r.git/': 403":                        "fatal: unable to access 'https://***@example.invalid/r.git/': 403",
		"fatal: could not read from 'git@example.invalid:org/r.git'":                                "fatal: could not read from 'git@example.invalid:org/r.git'",
		"fatal: could not read from 'ssh://tk@example.invalid/org/r.git'":                           "fatal: could not read from 'ssh://***@example.invalid/org/r.git'",
		"fatal: could not read from 'ssh://u:pw@example.invalid/org/r.git'":                         "fatal: could not read from 'ssh://***@example.invalid/org/r.git'",
		"fatal: unable to access 'git://tk@example.invalid/r.git' and 'ftp://tk@example.invalid/x'": "fatal: unable to access 'git://***@example.invalid/r.git' and 'ftp://***@example.invalid/x'",
		"no urls here": "no urls here",
	} {
		if got := RedactText(in); got != want {
			t.Errorf("RedactText(%q) = %q, want %q", in, got, want)
		}
	}
}
