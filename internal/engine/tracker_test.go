package engine

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// TestTrackerSettingValues covers the tracker and tracker_url parsers
// (§12.2): each legal value in its stored shape, and each refusal. A
// refused tracker_url is never quoted: it may hold a credential.
func TestTrackerSettingValues(t *testing.T) {
	for _, c := range []struct{ key, value, want string }{
		{SettingTracker, "none", `"none"`},
		{SettingTracker, "github", `"github"`},
		{SettingTracker, " Gitea ", `"gitea"`},
		{SettingTracker, "", ""},
		{SettingTrackerURL, "https://git.example.invalid", `"https://git.example.invalid"`},
		{SettingTrackerURL, " HTTP://Git.Example.invalid:3000/forge/ ", `"http://git.example.invalid:3000/forge"`},
		{SettingTrackerURL, "", ""},
		{SettingTrackerURL, "  ", ""},
	} {
		raw, err := parseSettingValue(c.key, c.value)
		if err != nil || string(raw) != c.want {
			t.Errorf("%s=%q: got %s, %v; want %s", c.key, c.value, raw, err, c.want)
		}
	}
	for _, c := range []struct{ key, value, want string }{
		{SettingTracker, "gitlab", "tracker takes none, github or gitea"},
		{SettingTracker, "auto", "tracker takes none, github or gitea"},
		{SettingTrackerURL, "https://sekrit-user:sekrit-pw@git.example.invalid", "must not carry a user name or password"},
		{SettingTrackerURL, "https://sekrit-user@git.example.invalid", "must not carry a user name or password"},
		{SettingTrackerURL, "ssh://sekrit-host.example.invalid/x", "takes an http:// or https:// URL"},
		{SettingTrackerURL, "file:///sekrit-path", "takes an http:// or https:// URL"},
		{SettingTrackerURL, "sekrit-host.example.invalid", "takes an http:// or https:// URL"},
		{SettingTrackerURL, "https://", "names no host"},
		{SettingTrackerURL, "https:sekrit-opaque", "names no host"},
		{SettingTrackerURL, "https://git.example.invalid/?token=sekrit-q", "must not carry a query or a fragment"},
		{SettingTrackerURL, "https://git.example.invalid/#sekrit-frag", "must not carry a query or a fragment"},
		{SettingTrackerURL, "https://sekrit-host.example.invalid:port/%zz", "is not a URL"},
	} {
		err := ValidateSettingValue(c.key, c.value)
		if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s=%q: err = %v, want ErrInvalidSetting saying %q", c.key, c.value, err, c.want)
			continue
		}
		if c.key == SettingTrackerURL && strings.Contains(err.Error(), "sekrit") {
			t.Errorf("%s=%q: the refusal quotes the URL: %v", c.key, c.value, err)
		}
	}
}

// TestTrackerSettingsRoundTrip covers set, show and reset of the two
// keys, the rule they share — gitea needs a URL, however the row gets
// there — and the reader the fetch uses.
func TestTrackerSettingsRoundTrip(t *testing.T) {
	e := newEnv(t)
	read := func() trackerSetting {
		t.Helper()
		var got trackerSetting
		if err := e.st.View(e.ctx, func(tx *sql.Tx) (err error) {
			got, err = projectTrackerTx(tx, e.project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); got != (trackerSetting{}) || got.Host() != "" {
		t.Errorf("unset tracker = %+v, want the zero value", got)
	}
	refused := func(values map[string]string) {
		t.Helper()
		_, err := e.eng.SetProjectSettings(e.ctx, envRepo, values)
		if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), "tracker gitea needs tracker_url") {
			t.Errorf("set %v: err = %v, want ErrInvalidSetting saying gitea needs tracker_url", values, err)
		}
	}
	refused(map[string]string{SettingTracker: "gitea"})
	if got := read(); got != (trackerSetting{}) {
		t.Errorf("a refused set stored %+v", got)
	}
	got := e.set(map[string]string{SettingTracker: "gitea", SettingTrackerURL: "https://Git.Example.invalid:3000/"})
	if s := settingOf(t, got, SettingTracker); s.Value != "gitea" || s.Default || s.Source != SourceLocal {
		t.Errorf("tracker = %+v, want a local gitea", s)
	}
	if s := settingOf(t, got, SettingTrackerURL); s.Value != "https://git.example.invalid:3000" || s.Default {
		t.Errorf("tracker_url = %+v, want the normalized URL", s)
	}
	if tr := read(); tr.Kind != TrackerGitea || tr.URL != "https://git.example.invalid:3000" || tr.Host() != "git.example.invalid:3000" {
		t.Errorf("stored tracker = %+v (host %q)", tr, tr.Host())
	}
	// The URL cannot be reset from under gitea; both together, or the
	// kind alone, can.
	refused(map[string]string{SettingTrackerURL: ""})
	got = e.set(map[string]string{SettingTracker: "github"})
	if s := settingOf(t, got, SettingTracker); s.Value != "github" {
		t.Errorf("tracker = %+v, want github", s)
	}
	got = e.set(map[string]string{SettingTracker: "none", SettingTrackerURL: ""})
	if s := settingOf(t, got, SettingTrackerURL); s.Value != "none" || !s.Default {
		t.Errorf("tracker_url after reset = %+v, want the none default", s)
	}
	got = e.set(map[string]string{SettingTracker: ""})
	if s := settingOf(t, got, SettingTracker); s.Value != "auto" || !s.Default || s.Source != SourceDefault {
		t.Errorf("tracker after reset = %+v, want the auto default", s)
	}
}

// TestTrackerProjectConfig covers the two keys in the shared file: they
// parse into the stored shape, refuse what `project set` refuses without
// quoting the URL, and an import whose merged row is gitea with no URL
// is refused whole.
func TestTrackerProjectConfig(t *testing.T) {
	cfg, err := ParseProjectConfig(cfgFile("tracker: gitea\ntracker_url: https://git.example.invalid:3000/forge/\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(cfg.Values[SettingTracker]); got != `"gitea"` {
		t.Errorf("tracker = %s, want \"gitea\"", got)
	}
	if got := string(cfg.Values[SettingTrackerURL]); got != `"https://git.example.invalid:3000/forge"` {
		t.Errorf("tracker_url = %s", got)
	}
	for body, want := range map[string]string{
		"tracker: gitlab\n":                                       "tracker takes none, github or gitea",
		"tracker: [github]\n":                                     "must be a string, not a list",
		"tracker_url: ssh://sekrit.example.invalid\n":             "takes an http:// or https:// URL",
		"tracker_url: https://sekrit:pw@git.example.invalid\n":    "must not carry a user name or password",
		"tracker_url: \"https://sekrit@git.example.invalid/x\"\n": "must not carry a user name or password",
	} {
		_, err := ParseProjectConfig(cfgFile(body))
		if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want ErrInvalidSetting saying %q", body, err, want)
		} else if strings.Contains(err.Error(), "sekrit") {
			t.Errorf("%q: the refusal quotes the URL: %v", body, err)
		}
	}

	e := newEnv(t)
	_, err = e.eng.ImportProjectConfig(e.ctx, envRepo, cfgFile("tracker: gitea\n"), true)
	if !errors.Is(err, ErrInvalidSetting) || !strings.Contains(err.Error(), "tracker gitea needs tracker_url") {
		t.Errorf("import of gitea with no URL: err = %v, want the refusal", err)
	}
	if raw := e.str("SELECT settings FROM projects WHERE id = ?", e.project); strings.Contains(raw, "gitea") {
		t.Errorf("a refused import stored %s", raw)
	}
	res, err := e.eng.ImportProjectConfig(e.ctx, envRepo, cfgFile("tracker: gitea\ntracker_url: https://git.example.invalid\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if s := settingOf(t, *res.Settings, SettingTracker); s.Value != "gitea" || s.Source != SourceImported {
		t.Errorf("imported tracker = %+v", s)
	}
	if s := settingOf(t, *res.Settings, SettingTrackerURL); s.Value != "https://git.example.invalid" || s.Source != SourceImported {
		t.Errorf("imported tracker_url = %+v", s)
	}
}

// TestTrackerRefusal pins the reasons issue_read gives for a tracker
// that names no forge (§12.2): none, and unset off GitHub. A tracker
// that names a forge, and an unset one on GitHub, are not refused here.
func TestTrackerRefusal(t *testing.T) {
	err := trackerRefusal(TrackerNone, true)
	if !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "the project's tracker setting is none, so no forge is read") {
		t.Errorf("tracker none: err = %v", err)
	}
	err = trackerRefusal("", false)
	if !errors.Is(err, ErrIssueRead) || !strings.Contains(err.Error(), "the repository is not on GitHub and the project sets no tracker") {
		t.Errorf("unset off GitHub: err = %v", err)
	}
	for _, c := range []struct {
		kind     string
		onGitHub bool
	}{{"", true}, {TrackerGitHub, false}, {TrackerGitHub, true}, {TrackerGitea, false}} {
		if err := trackerRefusal(c.kind, c.onGitHub); err != nil {
			t.Errorf("tracker %q (on GitHub %v) refused: %v", c.kind, c.onGitHub, err)
		}
	}
}

// TestTrackerTokenResolver covers the engine's side of the token: no
// resolver or no host is no token, and a configured one is asked by
// host[:port] at the moment of the call.
func TestTrackerTokenResolver(t *testing.T) {
	e := newEnv(t)
	if tok, ok, err := e.eng.trackerToken("git.example.invalid"); tok != "" || ok || err != nil {
		t.Errorf("no resolver: %q, %v, %v", tok, ok, err)
	}
	var asked []string
	e2 := newEnvCfg(t, func(c *Config) {
		c.TrackerToken = func(host string) (string, bool, error) {
			asked = append(asked, host)
			return "tok", true, nil
		}
	})
	if tok, ok, err := e2.eng.trackerToken(""); tok != "" || ok || err != nil {
		t.Errorf("no host: %q, %v, %v", tok, ok, err)
	}
	if tok, ok, err := e2.eng.trackerToken("git.example.invalid:3000"); tok != "tok" || !ok || err != nil {
		t.Errorf("resolved: %q, %v, %v", tok, ok, err)
	}
	if len(asked) != 1 || asked[0] != "git.example.invalid:3000" {
		t.Errorf("resolver asked %v", asked)
	}
}
