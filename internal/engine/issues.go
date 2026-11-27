package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Issue is one issue or pull request of the project's own repository,
// as issue_read answers it (§12.2): the number, title and body, and the
// comments when they were asked for. All of it is text from the forge —
// external, untrusted — which the tool renders inside <issue> data
// delimiters, never as instructions.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	// Author is who opened the issue, the forge's login.
	Author string `json:"author,omitempty"`
	// URL is the issue's page on the forge, when it gave one.
	URL string `json:"url,omitempty"`
	// PullRequest says the number named a pull request, not an issue:
	// the same numbers space on GitHub and Gitea either way.
	PullRequest bool `json:"pull_request,omitempty"`
	// State is the forge's own word for where the issue stands, open or
	// closed, when it gave one.
	State string `json:"state,omitempty"`
	// Labels are the names of the issue's labels, in the forge's order.
	Labels []string `json:"labels,omitempty"`
	// Comments are the asked-for comments, oldest first, as the forge
	// lists them.
	Comments []IssueComment `json:"comments,omitempty"`
}

// IssueComment is one comment on an issue or pull request (§12.2).
type IssueComment struct {
	Author string `json:"author,omitempty"`
	Body   string `json:"body"`
	// CreatedAt is when the forge says the comment landed, RFC3339.
	CreatedAt string `json:"created_at,omitempty"`
}

// ErrIssueRead marks every reason an issue could not be read that the
// foreman can report and the user can act on (§12.2): the repository is
// not on GitHub and the project sets no tracker (the tracker and
// tracker_url settings name a Gitea forge), gh is missing or not
// logged in, the project is gone.
// The API answers it as the caller's refusal with the reason in its
// message; the tool never falls back to searching anywhere else.
var ErrIssueRead = errors.New("issue unreadable")

// ErrIssueNotFound is the number that does not exist on the project's
// own repository (§12.2): its own reason, for the API answers it
// not-found while the rest answer the caller's mistake.
var ErrIssueNotFound = errors.New("no such issue or pull request on the project's repository")

// ReadIssue reads one issue or pull request of the project's own
// repository (§12.1 issues, §12.2 issue_read): number names it on the
// forge the project's repo URL names, comments asks for the comments
// too. The repository is never an argument — the project names it, and
// the caller names the project, so a foreman token's own project is the
// one repository it can read. Every reason the read fails comes back as
// an error the caller can report; nothing is searched as a fallback.
func (e *Engine) ReadIssue(ctx context.Context, projectID int64, number int, comments bool) (Issue, error) {
	if number <= 0 {
		return Issue{}, fmt.Errorf("read the issue: the number must be positive")
	}
	var repoURL string
	var tracker trackerSetting
	var missing bool
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		if _, err := e.sweepTx(tx, e.cfg.Now()); err != nil {
			return err
		}
		err := tx.QueryRow("SELECT repo_url FROM projects WHERE id = ?", projectID).Scan(&repoURL)
		if errors.Is(err, sql.ErrNoRows) {
			missing = true
			return nil
		}
		if err != nil {
			return err
		}
		tracker, err = projectTrackerTx(tx, projectID)
		return err
	})
	if err != nil {
		return Issue{}, fmt.Errorf("read the issue: read the project: %w", err)
	}
	if missing {
		return Issue{}, fmt.Errorf("%w: no such project", ErrIssueRead)
	}
	issue, err := e.fetchIssue(ctx, tracker, repoURL, number, comments)
	if err != nil {
		return Issue{}, err
	}
	return *issue, nil
}

// fetchIssue dispatches an issue read by the project's tracker setting
// (§12.2): none reads nothing, gitea reads the forge at tracker_url over
// its HTTP API, and github or unset read through IssueFetch, the gh seam.
// An unset tracker on a repository off GitHub is refused with the reason
// that names the setting; an explicit github one says the repository is
// not on GitHub.
func (e *Engine) fetchIssue(ctx context.Context, tracker trackerSetting, repoURL string, number int, comments bool) (*Issue, error) {
	switch tracker.Kind {
	case TrackerNone:
		return nil, trackerRefusal(TrackerNone, false)
	case TrackerGitea:
		return e.giteaIssue(ctx, tracker, repoURL, number, comments)
	}
	issue, err := e.cfg.IssueFetch(ctx, repoURL, number, comments)
	if tracker.Kind == "" && errors.Is(err, errNotOnGitHub) {
		return nil, trackerRefusal("", false)
	}
	return issue, err
}

// ghIssueFetch is the shipped issue fetch (§12.2): the engine reads the
// forge with the user's own gh login on its host, GET requests alone —
// no comment is posted, nothing changes. A repository not on GitHub, a
// gh that is missing or not logged in, or a number that does not exist
// comes back as the reason, an error the foreman can report.
func ghIssueFetch(ctx context.Context, repoURL string, number int, comments bool) (*Issue, error) {
	owner, repo, err := githubParts(repoURL)
	if err != nil {
		return nil, err
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return nil, fmt.Errorf("%w: gh is missing on the engine's host, so the forge cannot be read", ErrIssueRead)
	}
	data, err := ghGet(ctx, gh, fmt.Sprintf("repos/%s/%s/issues/%d", owner, repo, number))
	if err != nil {
		return nil, err
	}
	out, err := parseIssue(data, "gh's answer")
	if err != nil {
		return nil, err
	}
	if comments {
		data, err := ghGet(ctx, gh, fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, number))
		if err != nil {
			return nil, err
		}
		if out.Comments, err = parseIssueComments(data, "gh's answer"); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// parseIssue reads one issue as GitHub's and Gitea's REST APIs both
// answer it (§12.2): the same fields under the same names, so both
// forges fill an Issue alike. pull_request is an object on a pull
// request; GitHub leaves it out of an issue and Gitea sends null. from
// names the answer in a refusal.
func parseIssue(data []byte, from string) (*Issue, error) {
	var row struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest *struct{} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &row); err != nil {
		return nil, fmt.Errorf("%w: %s is not an issue: %v", ErrIssueRead, from, err)
	}
	out := &Issue{
		Number: row.Number, Title: row.Title, Body: row.Body,
		Author: row.User.Login, URL: row.HTMLURL, State: row.State,
		PullRequest: row.PullRequest != nil,
	}
	for _, l := range row.Labels {
		if l.Name != "" {
			out.Labels = append(out.Labels, l.Name)
		}
	}
	return out, nil
}

// parseIssueComments reads a comment list as GitHub's and Gitea's REST
// APIs both answer it, oldest first.
func parseIssueComments(data []byte, from string) ([]IssueComment, error) {
	var rows []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("%w: %s is not a comment list: %v", ErrIssueRead, from, err)
	}
	var out []IssueComment
	for _, c := range rows {
		out = append(out, IssueComment{Author: c.User.Login, Body: c.Body, CreatedAt: c.CreatedAt})
	}
	return out, nil
}

// errNotOnGitHub marks githubParts' refusal of a repository on another
// host, so an unset tracker can answer it with the reason that names the
// tracker setting.
var errNotOnGitHub = errors.New("the repository is not on GitHub")

// githubParts reads the owner and repository a project's repo URL names
// on GitHub (§12.2): the canonical identity of every GitHub form —
// https, ssh, scp-like — is github.com/<owner>/<repo>. Any other host,
// or a path that is not exactly one owner and repository, is a
// repository gh cannot read, and is refused as such.
func githubParts(repoURL string) (owner, repo string, err error) {
	parts, perr := parseRepo(repoURL)
	if perr != nil {
		return "", "", fmt.Errorf("%w: %v", ErrIssueRead, perr)
	}
	host, rest, ok := strings.Cut(parts.Canon, "/")
	if !ok || host != "github.com" {
		return "", "", fmt.Errorf("%w: %w", ErrIssueRead, errNotOnGitHub)
	}
	owner, repo, ok = strings.Cut(rest, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", fmt.Errorf("%w: the repository does not name one owner/repository on GitHub", ErrIssueRead)
	}
	return owner, repo, nil
}

// ghGet performs one GET through gh (§12.2): `gh api` with gh's own
// login, so the engine holds no forge credential, and the endpoint
// alone — no flags, no input — so the call can only read. Its failures
// are sorted into the reasons §12.2 names: a number that does not
// exist, and a gh that is missing or not logged in; the rest pass gh's
// own words through, bounded.
func ghGet(ctx context.Context, gh, endpoint string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, gh, "api", endpoint)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	msg := strings.TrimSpace(stderr.String())
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not found") || strings.Contains(lower, "404"):
		return nil, fmt.Errorf("%w (%s)", ErrIssueNotFound, msg)
	case strings.Contains(lower, "gh auth login") || strings.Contains(lower, "not logged in") || strings.Contains(lower, "authentication"):
		return nil, fmt.Errorf("%w: gh is not logged in on the engine's host", ErrIssueRead)
	default:
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%w: gh failed: %s", ErrIssueRead, boundNote(msg))
	}
}
