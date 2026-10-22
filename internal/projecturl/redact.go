package projecturl

import (
	"regexp"
	"strings"
)

// Redact returns raw with anything that may be a credential removed, for
// errors and logs about a URL: the userinfo (everything up to the last
// '@', so a password holding '/' or ':' goes too, and so does a token
// standing alone) and any query or fragment.
func Redact(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	prefix := ""
	if i := strings.Index(s, "://"); i >= 0 {
		prefix, s = s[:i+3], s[i+3:]
	}
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = "***@" + s[at+1:]
	}
	return prefix + s
}

// credURLRe matches the userinfo of a URL: scheme://user:pass@. The
// user and password parts cannot contain '/', ':' before the separator
// or '@', so the pattern stays anchored at the authority.
var credURLRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@:\s]+:[^/@\s]+@`)

// scpCredRe matches the userinfo of a scheme-less URL whose password
// holds a ':' or '/', such as user:1234/secret@host:org/repo: a run of
// non-space characters, with a ':' and then a '/', that ends in '@'.
var scpCredRe = regexp.MustCompile(`(^|[\s'"(])[^\s'"@/:]+:[^\s'"@/][^\s'"@]*/[^\s'"@]*@`)

// schemeUserRe matches the userinfo of a URL of any scheme that holds no
// ':' — a lone token (ssh://<token>@host, https://<token>@host) — up to
// the '@' that ends the authority.
var schemeUserRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s'"<>]+@`)

// httpURLRe matches an http or https URL inside a longer text: up to
// the next space, quote or bracket.
var httpURLRe = regexp.MustCompile(`(?i)https?://[^\s'"<>]+`)

// RedactText replaces any userinfo in the URLs a text quotes with ***:
// git mostly redacts credentials itself, but the runner and the plan
// mirror quote URLs and git output in their errors, and one leak through
// a job record or a log is one too many. A user:password pair goes, and
// so does a lone token (https://<token>@host, ssh://<token>@host) in a URL
// of any scheme, as does an http(s) URL's query or fragment. A scp-like
// git@host:path stays: its user is a login name, not a secret.
func RedactText(s string) string {
	s = credURLRe.ReplaceAllString(s, "$1***@")
	s = schemeUserRe.ReplaceAllString(s, "$1***@")
	s = scpCredRe.ReplaceAllString(s, "${1}***@")
	return httpURLRe.ReplaceAllStringFunc(s, Redact)
}
