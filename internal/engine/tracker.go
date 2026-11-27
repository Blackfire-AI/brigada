package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// The issue tracker of a project (§12.2 issue_read): which forge the
// engine reads issues from, and where it is. The settings name the forge
// alone; the token that reads it is machine config, a reference under
// tracker.<host>.token the engine resolves at fetch time and never puts
// in a job's environment, the store, a log or an error.
const (
	// SettingTracker is the forge's kind: none, github or gitea. Unset is
	// the behaviour before the setting existed: gh reads the repository
	// when its URL is on github.com.
	SettingTracker = "tracker"
	// SettingTrackerURL is the forge's base URL, http or https, with no
	// user name, password, query or fragment. tracker gitea needs it.
	SettingTrackerURL = "tracker_url"
)

// The values tracker takes.
const (
	TrackerNone   = "none"
	TrackerGitHub = "github"
	TrackerGitea  = "gitea"
)

// trackerUnsetShown is what show prints for an unset tracker: no value of
// the setting, the name of the behaviour an unset one keeps.
const trackerUnsetShown = "auto"

// parseTracker validates a tracker value: trimmed, lowercased, one of the
// three kinds.
func parseTracker(value string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case TrackerNone, TrackerGitHub, TrackerGitea:
		return v, nil
	}
	return "", fmt.Errorf("%w: %s takes %s, %s or %s, got %q (an empty value resets it: gh reads a repository on github.com)",
		ErrInvalidSetting, SettingTracker, TrackerNone, TrackerGitHub, TrackerGitea, value)
}

// parseTrackerURL validates a tracker_url and returns its stored form:
// scheme and host lowercased, the path kept without a trailing slash. It
// takes http and https alone and refuses a user name or password, a
// query and a fragment. A refusal never quotes the URL, which may hold a
// credential the user pasted by mistake.
func parseTrackerURL(value string) (string, error) {
	refuse := func(why string) (string, error) {
		return "", fmt.Errorf("%w: %s %s (the value is not shown: it may hold a credential)", ErrInvalidSetting, SettingTrackerURL, why)
	}
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		// url.Parse's own error quotes its input, so it is dropped.
		return refuse("is not a URL; give the forge's base URL, https://host[:port][/path]")
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme != "http" && scheme != "https":
		return refuse("takes an http:// or https:// URL")
	case u.User != nil:
		return refuse("must not carry a user name or password; the token is the machine config's tracker.<host>.token reference")
	case u.Opaque != "" || u.Hostname() == "":
		return refuse("names no host; give the forge's base URL, https://host[:port][/path]")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return refuse("must not carry a query or a fragment")
	}
	return scheme + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// trackerHost is the host[:port] of a stored tracker_url, lowercased: the
// <host> of the machine config key tracker.<host>.token. "" when the URL
// names none.
func trackerHost(trackerURL string) string {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// checkTrackerRow is the one rule the two tracker keys share, checked on
// the settings row as it would be stored, whichever write path built it:
// tracker gitea names no forge without tracker_url.
func checkTrackerRow(row map[string]json.RawMessage) error {
	kind, _ := rawString(row[SettingTracker])
	if kind != TrackerGitea {
		return nil
	}
	if u, _ := rawString(row[SettingTrackerURL]); u != "" {
		return nil
	}
	return fmt.Errorf("%w: %s %s needs %s, the forge's base URL; set both together (brigada project set %s=%s %s=https://host)",
		ErrInvalidSetting, SettingTracker, TrackerGitea, SettingTrackerURL, SettingTracker, TrackerGitea, SettingTrackerURL)
}

// trackerSetting is a project's tracker as stored: Kind is "" when the
// setting is unset, URL is the validated base URL or "".
type trackerSetting struct {
	Kind string
	URL  string
}

// Host is the host[:port] the tracker's token is configured under.
func (t trackerSetting) Host() string { return trackerHost(t.URL) }

// projectTrackerTx reads the project's tracker settings.
func projectTrackerTx(tx *sql.Tx, project int64) (trackerSetting, error) {
	var raw string
	if err := tx.QueryRow("SELECT settings FROM projects WHERE id = ?", project).Scan(&raw); err != nil {
		return trackerSetting{}, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	if raw == "" {
		return trackerSetting{}, nil
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &present); err != nil {
		return trackerSetting{}, fmt.Errorf("parse the settings of project %d: %w", project, err)
	}
	var t trackerSetting
	t.Kind, _ = rawString(present[SettingTracker])
	t.URL, _ = rawString(present[SettingTrackerURL])
	return t, nil
}

// trackerRefusal is why issue_read reads nothing for a tracker setting
// that names no forge (§12.2), nil for one that names a forge: tracker
// none is the user's own choice, and an unset tracker reads GitHub alone,
// so onGitHub says whether the project's repository is there. Each is
// ErrIssueRead, a reason the foreman reports and the user can act on.
func trackerRefusal(kind string, onGitHub bool) error {
	switch {
	case kind == TrackerNone:
		return fmt.Errorf("%w: the project's %s setting is %s, so no forge is read; set it to %s or %s to read issues (brigada project set %s=...)",
			ErrIssueRead, SettingTracker, TrackerNone, TrackerGitHub, TrackerGitea, SettingTracker)
	case kind == "" && !onGitHub:
		return fmt.Errorf("%w: the repository is not on GitHub and the project sets no %s; name its forge to read issues (brigada project set %s=%s %s=https://host)",
			ErrIssueRead, SettingTracker, SettingTracker, TrackerGitea, SettingTrackerURL)
	}
	return nil
}

// trackerToken resolves the token configured for the forge at host
// (host[:port]) through the machine config's tracker.<host>.token
// reference, at the moment of the call. ok is false when no reference is
// configured, which is a forge read without a token. The token goes to
// the forge request alone: never into a job's environment, the store, a
// log or an error.
func (e *Engine) trackerToken(host string) (token string, ok bool, err error) {
	if e.cfg.TrackerToken == nil || host == "" {
		return "", false, nil
	}
	return e.cfg.TrackerToken(host)
}
