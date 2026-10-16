// Package projecturl names repositories by the canonical identity of design §13.1.
package projecturl

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Canonical removes transport credentials and transport-specific spelling. It
// refuses local paths and SSH aliases whose identity needs user confirmation.
// Errors deliberately never quote input: a remote may contain a password.
func Canonical(raw string) (string, error) {
	bad := func() (string, error) {
		return "", fmt.Errorf("repository URL is local or ambiguous; run brigada init to select its canonical URL")
	}
	raw = strings.TrimSpace(raw)
	var host, port, path, scheme string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
			return bad()
		}
		scheme = strings.ToLower(u.Scheme)
		switch scheme {
		case "https", "http", "ssh", "git":
		default:
			return bad()
		}
		host, port, path = u.Hostname(), u.Port(), strings.TrimPrefix(u.Path, "/")
	} else {
		// scp-like SSH has a user and a colon; the canonical spelling has a slash.
		if at := strings.LastIndex(raw, "@"); at >= 0 {
			rest := raw[at+1:]
			colon := strings.Index(rest, ":")
			if colon < 1 {
				return bad()
			}
			host, path, scheme = rest[:colon], rest[colon+1:], "ssh"
			if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~") {
				return bad()
			}
		} else {
			slash := strings.Index(raw, "/")
			if slash < 1 {
				return bad()
			}
			host, path = raw[:slash], raw[slash+1:]
			if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
				host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
			} else if strings.Contains(host, ":") {
				var err error
				host, port, err = net.SplitHostPort(host)
				if err != nil {
					return bad()
				}
			}
		}
	}
	host = strings.ToLower(host)
	// A bare hostname could be an SSH config alias. Do not resolve it by guess.
	if host == "" || (!strings.Contains(host, ".") && net.ParseIP(host) == nil) || strings.ContainsAny(host, "@ /\\") {
		return bad()
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return bad()
		}
	}
	defaults := map[string]string{"ssh": "22", "https": "443", "http": "80", "git": "9418"}
	if port != "" && port == defaults[scheme] {
		port = ""
	}
	path = strings.TrimRight(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~") || strings.ContainsAny(path, "\\\r\n\t ") {
		return bad()
	}
	for _, p := range strings.Split(path, "/") {
		if p == "" || p == "." || p == ".." {
			return bad()
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return host + "/" + path, nil
}
