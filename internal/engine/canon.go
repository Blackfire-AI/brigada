package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// ErrInvalidRepoURL marks a repo URL the engine will not take: empty,
// malformed, or ambiguous in a way the string alone cannot settle
// (design §13.1 item 1). The message names the problem and never quotes
// the URL, which may carry a credential.
var ErrInvalidRepoURL = errors.New("invalid repo URL")

func badRepo(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRepoURL, fmt.Sprintf(format, args...))
}

// defaultPorts are the ports a scheme implies; a canonical URL drops them.
var defaultPorts = map[string]int{
	"http":    80,
	"https":   443,
	"ssh":     22,
	"git+ssh": 22,
	"ssh+git": 22,
	"git":     9418,
}

var (
	schemeRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
	scpUserRe  = regexp.MustCompile(`^[A-Za-z0-9._~%+-]+$`)
	hostRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
	portPathRe = regexp.MustCompile(`^([0-9]+)/(.*)$`)
)

// repoParts is a parsed repo URL: the canonical identity, and the
// credential-free URL a runner can fetch with.
type repoParts struct {
	// Canon is the identity: host[:port]/path, or file:<abs path>.
	Canon string
	// Transport is a usable Git URL with any password or token dropped;
	// empty when the string names an identity only (host/path).
	Transport string
}

// Canonicalize turns a repo URL into the project's canonical identity
// (design §13.1 item 1): scp-like SSH, ssh:// and HTTPS forms of one host
// and path give host[:port]/path. Userinfo is dropped and never appears in
// the result or in an error; the host is lowercased and the path is not;
// one trailing .git and trailing slashes go; a port stays only when it is
// not the scheme's default. A file:// remote, or a plain absolute path,
// gives file:<absolute cleaned path>. What the string alone cannot settle
// (a leading-slash or ~ scp path, an SSH config alias, a '@' in a path
// that may be an unescaped password) is an error, never a guess.
func Canonicalize(rawURL string) (string, error) {
	p, err := parseRepo(rawURL)
	if err != nil {
		return "", err
	}
	return p.Canon, nil
}

// RedactRepoURL returns raw with anything that may be a credential
// removed, for errors and logs about input that failed to parse: the
// userinfo (everything up to the last '@', so a password holding '/' or
// ':' goes too) and any query or fragment.
func RedactRepoURL(raw string) string {
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

func parseRepo(rawURL string) (repoParts, error) {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return repoParts{}, badRepo("the repo URL is empty")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return repoParts{}, badRepo("the repo URL holds a control character")
		}
	}
	if i := strings.Index(s, "://"); i > 0 && schemeRe.MatchString(s[:i]) {
		return parseSchemeRepo(strings.ToLower(s[:i]), s[i+3:])
	}
	if rest, ok := cutFold(s, "file:"); ok {
		return fileRepo(rest)
	}
	if strings.HasPrefix(s, "/") {
		return fileRepo(s)
	}
	if strings.HasPrefix(s, "~") {
		return repoParts{}, badRepo("a path starting with ~ is ambiguous (a home directory or an scp-like path?); give an absolute path or a full URL")
	}
	return parseScpRepo(s)
}

func cutFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// fileRepo canonicalizes a local path remote: file:<absolute cleaned
// path>. A file:// URL may name the host "localhost" or none.
func fileRepo(rest string) (repoParts, error) {
	if strings.HasPrefix(rest, "//") {
		rest = rest[2:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return repoParts{}, badRepo("a file:// URL names no path")
		}
		if host := rest[:slash]; host != "" && !strings.EqualFold(host, "localhost") {
			return repoParts{}, badRepo("a file:// URL names a host; only a local path is a file remote")
		}
		rest = rest[slash:]
		if dec, err := url.PathUnescape(rest); err == nil {
			rest = dec
		}
	}
	if !strings.HasPrefix(rest, "/") {
		return repoParts{}, badRepo("a file remote must be an absolute path")
	}
	clean := path.Clean(rest)
	if clean == "/" {
		return repoParts{}, badRepo("a file remote names the filesystem root, not a repository")
	}
	return repoParts{Canon: "file:" + clean, Transport: "file://" + clean}, nil
}

// splitHostPort validates "host" or "host:port" (IPv6 in brackets) and
// returns the lowercased host and the port, 0 when none.
func splitHostPort(hp string) (host string, port int, err error) {
	if hp == "" {
		return "", 0, badRepo("the URL names no host")
	}
	rest := ""
	if strings.HasPrefix(hp, "[") {
		end := strings.Index(hp, "]")
		if end < 0 {
			return "", 0, badRepo("the host's IPv6 bracket is not closed")
		}
		host, rest = strings.ToLower(hp[:end+1]), hp[end+1:]
		if !regexp.MustCompile(`^\[[0-9a-f:.]+\]$`).MatchString(host) {
			return "", 0, badRepo("the host is not a valid IPv6 address")
		}
		if rest != "" && !strings.HasPrefix(rest, ":") {
			return "", 0, badRepo("the host is followed by something other than a port")
		}
	} else {
		host = hp
		if i := strings.Index(hp, ":"); i >= 0 {
			host, rest = hp[:i], hp[i:]
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if !hostRe.MatchString(host) {
			return "", 0, badRepo("the host is not a valid hostname")
		}
		if !strings.Contains(host, ".") && host != "localhost" {
			return "", 0, badRepo("the host is a single word, which may be an SSH config alias rather than a hostname; give the full hostname")
		}
	}
	if rest != "" {
		ps := rest[1:]
		n, perr := strconv.Atoi(ps)
		if ps == "" || perr != nil || n < 1 || n > 65535 || strings.TrimLeft(ps, "0123456789") != "" {
			return "", 0, badRepo("the port is not a number from 1 to 65535")
		}
		port = n
	}
	return host, port, nil
}

// cleanRepoPath strips trailing slashes and, for a URL as given, one
// trailing .git, and returns the path's segments; "." and ".." segments
// are refused. The canonical identity form is taken as it stands: a
// second .git strip would turn repo.git.git into repo.git and then
// repo, and the identity must be a fixed point.
func cleanRepoPath(p string, stripGit bool) ([]string, error) {
	p = strings.TrimRight(p, "/")
	if stripGit {
		p = strings.TrimSuffix(p, ".git")
	}
	p = strings.TrimRight(p, "/")
	var segs []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
		case ".", "..":
			return nil, badRepo("the path holds a '.' or '..' segment")
		default:
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return nil, badRepo("the URL names no repository path")
	}
	return segs, nil
}

func joinCanon(host string, port int, segs []string) string {
	hp := host
	if port != 0 {
		hp += ":" + strconv.Itoa(port)
	}
	return hp + "/" + strings.Join(segs, "/")
}

func parseSchemeRepo(scheme, rest string) (repoParts, error) {
	if strings.EqualFold(scheme, "file") {
		return fileRepo("//" + rest)
	}
	def, ok := defaultPorts[scheme]
	if !ok {
		return repoParts{}, badRepo("the scheme is not one of http, https, ssh, git or file")
	}
	if strings.ContainsAny(rest, " \t") {
		return repoParts{}, badRepo("the URL holds whitespace")
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		return repoParts{}, badRepo("the URL names no repository path")
	}
	authority, tail := rest[:end], rest[end:]
	if strings.ContainsAny(tail, "?#") {
		return repoParts{}, badRepo("a query or fragment in a repo URL is not supported")
	}
	// A '@' in the path may be the tail of an unescaped password
	// (user:1234/secret@host/...): the authority would then look like a
	// host and port. Refuse before any port is read.
	if strings.Contains(tail, "@") {
		return repoParts{}, badRepo("the path holds a '@', which may be a credential with an unescaped '/'; percent-encode credentials or drop them")
	}
	hostport, user := authority, ""
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		user, hostport = authority[:at], authority[at+1:]
	}
	if strings.ContainsAny(hostport, " \t") {
		return repoParts{}, badRepo("the host holds whitespace")
	}
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return repoParts{}, err
	}
	segs, err := cleanRepoPath(tail, true)
	if err != nil {
		return repoParts{}, err
	}
	canonPort := port
	if port == def {
		canonPort = 0
	}
	transport := scheme + "://"
	if scheme == "ssh" || scheme == "git+ssh" || scheme == "ssh+git" {
		// A user name rides ssh; a password never does.
		if name, _, _ := strings.Cut(user, ":"); name != "" && scpUserRe.MatchString(name) {
			transport += name + "@"
		}
	}
	transport += hostport + "/" + strings.TrimLeft(tail, "/")
	return repoParts{Canon: joinCanon(host, canonPort, segs), Transport: transport}, nil
}

func parseScpRepo(s string) (repoParts, error) {
	if strings.ContainsAny(s, " \t") {
		return repoParts{}, badRepo("the URL holds whitespace")
	}
	user, rest := "", s
	if at := strings.LastIndex(s, "@"); at >= 0 {
		user, rest = s[:at], s[at+1:]
		// An scp-like user is a plain name. A ':' or '/' in it is a
		// password the string would otherwise parse around (the part
		// before the '@' holding a fake host and port): refuse.
		if !scpUserRe.MatchString(user) {
			return repoParts{}, badRepo("the user part of an scp-like URL must be a plain user name; a ':', '/' or '@' there looks like a credential")
		}
	}
	var hostpart, tail string
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 || end+1 >= len(rest) || (rest[end+1] != ':' && rest[end+1] != '/') {
			return repoParts{}, badRepo("not a repo URL: expected host:path")
		}
		if rest[end+1] == '/' {
			if user != "" {
				return repoParts{}, badRepo("not a repo URL: expected host:path")
			}
			return identityRepo(rest[:end+1], rest[end+1:])
		}
		hostpart, tail = rest[:end+1], rest[end+2:]
		if user == "" {
			if m := portPathRe.FindStringSubmatch(tail); m != nil {
				return identityRepo(hostpart+":"+m[1], "/"+m[2])
			}
		}
	} else {
		i := strings.Index(rest, ":")
		if i < 0 {
			// host/path: the canonical identity itself, which must
			// re-canonicalize to itself (it names no transport).
			slash := strings.Index(rest, "/")
			if user != "" || slash <= 0 {
				return repoParts{}, badRepo("not a repo URL: expected a scheme, host:path or an absolute path (a relative path or an SSH config alias is ambiguous)")
			}
			if h := rest[:slash]; (!strings.Contains(h, ".") && h != "localhost") || strings.HasPrefix(h, ".") {
				return repoParts{}, badRepo("not a repo URL: a relative path is ambiguous (a directory or an SSH config alias?); give an absolute path or a full URL")
			}
			return identityRepo(rest[:slash], rest[slash:])
		}
		hostpart, tail = rest[:i], rest[i+1:]
		if strings.Contains(hostpart, "/") {
			return repoParts{}, badRepo("a relative path holding ':' is ambiguous")
		}
		if user == "" {
			// host:port/path is the canonical identity with a port.
			if m := portPathRe.FindStringSubmatch(tail); m != nil {
				return identityRepo(hostpart+":"+m[1], "/"+m[2])
			}
		}
	}
	host, _, err := splitHostPort(hostpart)
	if err != nil {
		return repoParts{}, err
	}
	if strings.HasPrefix(tail, "/") {
		return repoParts{}, badRepo("an scp-like path starting with '/' is ambiguous (an absolute path or an ssh:// URL?); write ssh://host/path")
	}
	if strings.HasPrefix(tail, "~") {
		return repoParts{}, badRepo("an scp-like path starting with ~ is ambiguous; write ssh://host/~user/path")
	}
	segs, err := cleanRepoPath(tail, true)
	if err != nil {
		return repoParts{}, err
	}
	transport := hostpart + ":" + tail
	if user != "" {
		transport = user + "@" + transport
	}
	// scp-like means SSH, whose default port the form cannot override.
	return repoParts{Canon: joinCanon(host, 0, segs), Transport: transport}, nil
}

// identityRepo parses a canonical host[:port]/path string.
func identityRepo(hostport, tail string) (repoParts, error) {
	if strings.Contains(tail, "@") {
		return repoParts{}, badRepo("the path holds a '@', which may be a credential with an unescaped '/'; percent-encode credentials or drop them")
	}
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return repoParts{}, err
	}
	segs, err := cleanRepoPath(tail, false)
	if err != nil {
		return repoParts{}, err
	}
	return repoParts{Canon: joinCanon(host, port, segs)}, nil
}

// lookupProjectTx finds the project a repo URL names: by its canonical
// form, else by the stored string itself (a legacy row that startup could
// not rewrite). sql.ErrNoRows when none; an unparsable URL is
// ErrInvalidRepoURL unless a row stores exactly that string.
func lookupProjectTx(tx *sql.Tx, raw, cols string, dest ...any) error {
	p, perr := parseRepo(raw)
	if perr == nil {
		err := tx.QueryRow("SELECT "+cols+" FROM projects WHERE repo_url = ?", p.Canon).Scan(dest...)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	err := tx.QueryRow("SELECT "+cols+" FROM projects WHERE repo_url = ?", strings.TrimSpace(raw)).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) && perr != nil {
		return perr
	}
	return err
}

// LookupProjectTx returns the id of the project a repo URL names, in the
// caller's transaction. sql.ErrNoRows when there is none.
func LookupProjectTx(tx *sql.Tx, raw string) (int64, error) {
	var id int64
	err := lookupProjectTx(tx, raw, "id", &id)
	return id, err
}
