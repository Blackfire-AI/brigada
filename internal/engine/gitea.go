package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// giteaTimeout bounds one Gitea read when the engine was given no client
// of its own.
const giteaTimeout = 30 * time.Second

// defaultGiteaClient is the client a gitea tracker is read with when
// Config.TrackerHTTP is nil.
var defaultGiteaClient = &http.Client{Timeout: giteaTimeout}

// giteaIssue reads one issue or pull request from the gitea forge the
// project's tracker_url names (§12.2 issue_read). The token comes from the
// machine config's tracker.<host>.token reference, resolved now; with no
// reference the forge is read without one.
func (e *Engine) giteaIssue(ctx context.Context, tracker trackerSetting, repoURL string, number int, comments bool) (*Issue, error) {
	host := tracker.Host()
	if tracker.URL == "" || host == "" {
		// The settings refuse gitea without a URL; a row that holds one
		// anyway is refused here too.
		return nil, fmt.Errorf("%w: the project's %s setting is %s but %s is unset", ErrIssueRead, SettingTracker, TrackerGitea, SettingTrackerURL)
	}
	token, _, err := e.trackerToken(host)
	if err != nil {
		// ResolveTrackerToken names the key and the reference, never a
		// value, so its reason can be passed on.
		return nil, fmt.Errorf("%w: the token for %s could not be read: %s", ErrIssueRead, host, boundNote(err.Error()))
	}
	client := e.cfg.TrackerHTTP
	if client == nil {
		client = defaultGiteaClient
	}
	return giteaIssueFetch(ctx, client, tracker.URL, token, repoURL, number, comments)
}

// giteaIssueFetch is the Gitea fetch (§12.2): GET requests alone against
// base's API, /api/v1/repos/{owner}/{repo}/issues/{n} and its comments
// when asked, with `Authorization: token <t>` when a token is given. The
// owner and repository are the last two segments of the project's repo URL
// path. A number that does not exist is ErrIssueNotFound; everything else
// that fails is ErrIssueRead with a bounded reason that never holds the
// token.
func giteaIssueFetch(ctx context.Context, client *http.Client, base, token, repoURL string, number int, comments bool) (*Issue, error) {
	owner, repo, err := giteaParts(repoURL)
	if err != nil {
		return nil, err
	}
	g := giteaReader{client: client, base: strings.TrimRight(base, "/"), token: token, host: trackerHost(base)}
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d", url.PathEscape(owner), url.PathEscape(repo), number)
	data, err := g.get(ctx, path)
	if err != nil {
		return nil, err
	}
	out, err := parseIssue(data, "the forge's answer")
	if err != nil {
		return nil, err
	}
	if comments {
		data, err := g.get(ctx, path+"/comments")
		if err != nil {
			return nil, err
		}
		if out.Comments, err = parseIssueComments(data, "the forge's answer"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// giteaParts reads the owner and repository a project's repo URL names on
// its forge: the last two segments of its canonical path, any .git
// dropped, so a forge served under a path prefix reads alike.
func giteaParts(repoURL string) (owner, repo string, err error) {
	parts, perr := parseRepo(repoURL)
	if perr != nil {
		return "", "", fmt.Errorf("%w: %v", ErrIssueRead, perr)
	}
	_, path, ok := strings.Cut(parts.Canon, "/")
	if strings.HasPrefix(parts.Canon, "file:") {
		ok = false
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if !ok || len(segs) < 2 || segs[len(segs)-2] == "" || segs[len(segs)-1] == "" {
		return "", "", fmt.Errorf("%w: the repository does not name an owner/repository on a forge", ErrIssueRead)
	}
	return segs[len(segs)-2], strings.TrimSuffix(segs[len(segs)-1], ".git"), nil
}

// giteaReader performs the GET requests of one Gitea read.
type giteaReader struct {
	client *http.Client
	base   string
	token  string
	host   string
}

// get reads one API path and sorts its failures into the reasons §12.2
// names. No reason quotes the request URL or a header, and the token is
// scrubbed from anything the forge sends back.
func (g giteaReader) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: the request to the forge at %s could not be built", ErrIssueRead, g.host)
	}
	req.Header.Set("Accept", "application/json")
	if g.token != "" {
		req.Header.Set("Authorization", "token "+g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		// A *url.Error quotes the URL; its cause alone is the reason.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("%w: the forge at %s could not be reached: %s", ErrIssueRead, g.host, g.scrub(err.Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: the forge at %s answered unreadably: %s", ErrIssueRead, g.host, g.scrub(err.Error()))
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return data, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w (the forge at %s answered 404)", ErrIssueNotFound, g.host)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: the forge at %s refused the read (HTTP %d); check the machine config's tracker.%s.token",
			ErrIssueRead, g.host, resp.StatusCode, g.host)
	}
	reason := "HTTP " + strconv.Itoa(resp.StatusCode)
	if msg := g.scrub(giteaMessage(data)); msg != "" {
		reason += ": " + msg
	}
	return nil, fmt.Errorf("%w: the forge at %s answered %s", ErrIssueRead, g.host, reason)
}

// scrub bounds a reason and drops the token from it.
func (g giteaReader) scrub(s string) string {
	if g.token != "" {
		s = strings.ReplaceAll(s, g.token, "[token]")
	}
	return boundNote(strings.TrimSpace(s))
}

// giteaMessage is the forge's own words in an error answer: Gitea's
// {"message": ...} when it sent one, the body otherwise.
func giteaMessage(data []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) == nil && body.Message != "" {
		return body.Message
	}
	return string(data)
}
