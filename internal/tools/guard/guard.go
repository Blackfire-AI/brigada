package main

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Rule ids, shown in findings as `path:line: rule: excerpt`.
const (
	ruleIP   = "private-ip"
	ruleHost = "private-host"
	ruleCred = "credential"
)

// finding is one detected leak in one tracked file.
type finding struct {
	path    string
	line    int
	rule    string
	excerpt string
}

// String renders the finding as one output line.
func (f finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.path, f.line, f.rule, f.excerpt)
}

// ipRange is an IPv4 range: a network base and a prefix length.
type ipRange struct {
	base uint32
	bits uint
}

func (r ipRange) contains(ip uint32) bool {
	mask := ^uint32(0) << (32 - r.bits)
	return ip&mask == r.base
}

// specialIPv4 are the ranges whose literals must not appear in the
// repository: private use (10/8, 172.16/12 and 192.168/16), link-local
// (169.254/16) and carrier-grade NAT (100.64/10). The bases are written as
// shifted integers rather than dotted literals so the guard never flags its
// own source.
var specialIPv4 = []ipRange{
	{10 << 24, 8},
	{100<<24 | 64<<16, 10},
	{169<<24 | 254<<16, 16},
	{172<<24 | 16<<16, 12},
	{192<<24 | 168<<16, 16},
}

// allowedIPv4 never produce findings: loopback (127/8) and the
// documentation blocks (192.0.2/24, 198.51.100/24 and 203.0.113/24), which
// examples and tests may use freely. The exemption is explicit so widening
// the table above cannot start failing documentation addresses.
var allowedIPv4 = []ipRange{
	{127 << 24, 8},
	{192<<24 | 2<<8, 24},
	{198<<24 | 51<<16 | 100<<8, 24},
	{203<<24 | 113<<8, 24},
}

// ipRe matches IPv4-shaped runs; the octets are validated after the match.
var ipRe = regexp.MustCompile(`\d{1,3}(?:\.\d{1,3}){3}`)

// hostRe matches hostname-shaped tokens: dot-separated labels of letters,
// digits and hyphens, none starting or ending with a hyphen.
var hostRe = regexp.MustCompile(`[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)+`)

// privateHostSuffixes mark a hostname as belonging to a private network.
var privateHostSuffixes = []string{".home.arpa", ".internal", ".lan", ".local"}

// credentialRes match issued token and key shapes. Every prefix requires a
// long body after it, so the prefixes named in these patterns do not make
// the guard flag its own source.
var credentialRes = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:ghp|gho|ghs)_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{16,}\b`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[abpr]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
}

// leak is one match inside one line, with the byte range it spans.
type leak struct {
	rule       string
	start, end int
}

// scanLine reports every leak in one line of text, ordered by position.
func scanLine(text string) []leak {
	var leaks []leak
	for _, m := range ipRe.FindAllStringIndex(text, -1) {
		if !dottedRunBoundary(text, m[0], m[1]) {
			continue
		}
		ip, ok := parseIPv4(text[m[0]:m[1]])
		if !ok || containsIP(allowedIPv4, ip) || !containsIP(specialIPv4, ip) {
			continue
		}
		leaks = append(leaks, leak{ruleIP, m[0], m[1]})
	}
	for _, m := range hostRe.FindAllStringIndex(text, -1) {
		name := strings.ToLower(text[m[0]:m[1]])
		for _, suffix := range privateHostSuffixes {
			if strings.HasSuffix(name, suffix) {
				leaks = append(leaks, leak{ruleHost, m[0], m[1]})
				break
			}
		}
	}
	for _, re := range credentialRes {
		for _, m := range re.FindAllStringIndex(text, -1) {
			leaks = append(leaks, leak{ruleCred, m[0], m[1]})
		}
	}
	sort.Slice(leaks, func(i, j int) bool { return leaks[i].start < leaks[j].start })
	return leaks
}

// dottedRunBoundary reports whether the candidate at text[start:end] is
// not glued to a longer run of digits and dots, which would make it part
// of a version string rather than an address. A neighbouring dot only
// counts as glue when it continues that run, so sentence punctuation
// around the address is left alone.
func dottedRunBoundary(text string, start, end int) bool {
	if start > 0 {
		prev := text[start-1]
		if isDigit(prev) || (prev == '.' && start >= 2 && isDigit(text[start-2])) {
			return false
		}
	}
	if end < len(text) {
		next := text[end]
		if isDigit(next) || (next == '.' && end+1 < len(text) && isDigit(text[end+1])) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// parseIPv4 parses dot-separated decimal octets; leading zeros count as
// decimal.
func parseIPv4(s string) (uint32, bool) {
	var ip uint32
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n > 255 {
			return 0, false
		}
		ip = ip<<8 | uint32(n)
	}
	return ip, true
}

func containsIP(ranges []ipRange, ip uint32) bool {
	for _, r := range ranges {
		if r.contains(ip) {
			return true
		}
	}
	return false
}

// edgeSpace is the whitespace TrimSpace would trim at the ends of a
// line; \n cannot occur because lines are already split on it.
const edgeSpace = " \t\r\v\f"

// excerptOf renders the match with a little context around it. When
// redact is set the match itself is reduced to its head: CI logs are
// public, so the guard must not republish the secret it caught.
func excerptOf(text string, start, end int, redact bool) string {
	const pad = 24
	lo := max(0, start-pad)
	hi := min(len(text), end+pad)
	match := text[start:end]
	if redact {
		match = redacted(match)
	}
	var b strings.Builder
	if lo > 0 {
		b.WriteString("…")
	}
	b.WriteString(strings.TrimLeft(text[lo:start], edgeSpace))
	b.WriteString(match)
	b.WriteString(strings.TrimRight(text[end:hi], edgeSpace))
	if hi < len(text) {
		b.WriteString("…")
	}
	return b.String()
}

// redacted keeps the head of a secret so the excerpt still identifies
// the finding.
func redacted(s string) string {
	const keep = 4
	return s[:min(len(s), keep)] + "…"
}

// exception drops findings in one file whose line matches its regexp.
type exception struct {
	path string
	re   *regexp.Regexp
}

// allows reports whether the exception covers a finding on line of path.
func (e exception) allows(path, line string) bool {
	return path == e.path && e.re.MatchString(line)
}

// parseAllowlist parses `path:regexp` lines. Blank lines and # comments are
// ignored; anything else is an error, so a typo cannot silently widen the
// guard.
func parseAllowlist(data string) ([]exception, error) {
	var exs []exception
	for n, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path, pattern, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: want `path:regexp`, got %q", n+1, line)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", n+1, err)
		}
		exs = append(exs, exception{path, re})
	}
	return exs, nil
}

// scanFile reports every finding in one file. Content containing a NUL
// byte is treated as binary and skipped.
func scanFile(path string, data []byte, exs []exception) []finding {
	if bytes.IndexByte(data, 0) >= 0 {
		return nil
	}
	var findings []finding
	for n, line := range strings.Split(string(data), "\n") {
		for _, lk := range scanLine(line) {
			allowed := false
			for _, ex := range exs {
				if ex.allows(path, line) {
					allowed = true
					break
				}
			}
			if !allowed {
				// The PEM header names the key; it is not the
				// secret itself, so it stays unredacted.
				redact := lk.rule == ruleCred && !strings.HasPrefix(line[lk.start:lk.end], "-----BEGIN")
				findings = append(findings, finding{
					path:    path,
					line:    n + 1,
					rule:    lk.rule,
					excerpt: excerptOf(line, lk.start, lk.end, redact),
				})
			}
		}
	}
	return findings
}
