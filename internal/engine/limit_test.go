package engine

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/notify"
)

// limitForeman parks a task, leases the foreman turn it woke and
// completes it with a limit error carrying until (RFC3339, "" for
// unknown) — the foreman hitting its subscription limit (§4.4).
func (e *env) limitForeman(until string) {
	e.t.Helper()
	e.park("which file?")
	g := e.leaseForeman()
	if ans := e.complete(g, Completion{Error: "foreman CLI failed (limit): you've hit your usage limit", ErrorKind: "limit", LimitUntil: until}); ans != AnsOK {
		e.t.Fatalf("complete the limited turn: %s", ans)
	}
}

func (e *env) limitedState() string {
	e.t.Helper()
	return e.str("SELECT foreman_limited FROM projects WHERE id = ?", e.project)
}

// TestForemanLimitHitMarksTheProject is §4.4: a limit-kind completion of
// the foreman's turn marks the project limited, appends the
// foreman_limited event, wakes nothing, and the user's notification
// lands through the delivery loop.
func TestForemanLimitHitMarksTheProject(t *testing.T) {
	e := newEnv(t)
	until := e.clock.Now().Add(2 * time.Hour)
	e.limitForeman(until.Format(time.RFC3339))

	if got, want := e.limitedState(), fmtTime(until); got != want {
		t.Errorf("limited state = %q, want %q", got, want)
	}
	if got := e.countEvents(evForemanLimited); got != 1 {
		t.Errorf("foreman_limited events = %d, want 1", got)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns = %d, want 1: a limited foreman is not woken again", got)
	}
	if got := e.inboxStates(); len(got) != 1 || !strings.HasPrefix(got[0], "pending:") {
		t.Errorf("inbox = %v, want the event pending", got)
	}

	fake := &notify.Fake{}
	if err := notify.NewLoop(e.st, fake, nil).Flush(e.ctx); err != nil {
		t.Fatalf("flush notifications: %v", err)
	}
	var found bool
	for _, n := range fake.Notes() {
		if strings.Contains(n.Body, "foreman hit its limit") && strings.Contains(n.Body, fmtTime(until)) {
			found = true
		}
	}
	if !found {
		t.Errorf("notes %v, want one naming the foreman's limit and its reset", fake.Notes())
	}
}

// TestForemanLimitUnknownUntil: a limit error with no usable reset time
// is limited(unknown), and the event carries no until.
func TestForemanLimitUnknownUntil(t *testing.T) {
	for name, until := range map[string]string{
		"empty":     "",
		"malformed": "tomorrow-ish",
		"passed":    time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC).Format(time.RFC3339),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.limitForeman(until)
			if got := e.limitedState(); got != foremanLimitUnknown {
				t.Errorf("limited state = %q, want %q", got, foremanLimitUnknown)
			}
			if got := e.str("SELECT payload FROM events WHERE kind = ?", evForemanLimited); strings.Contains(got, "until") {
				t.Errorf("event payload %s names an until, want none", got)
			}
			if got := e.foremanJobs(); got != 1 {
				t.Errorf("foreman turns = %d, want 1", got)
			}
		})
	}
}

// TestTaskJobLimitMarksNothing: only the foreman's key marks the
// project; a task worker's limit-kind completion is an ordinary failure.
func TestTaskJobLimitMarksNothing(t *testing.T) {
	e := newEnv(t)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if g == nil {
		t.Fatal("the work job was not leased")
	}
	e.complete(g, Completion{Error: "worker CLI failed (limit): quota", ErrorKind: "limit"})
	if got := e.limitedState(); got != "" {
		t.Errorf("limited state = %q, want none", got)
	}
	if got := e.countEvents(evForemanLimited); got != 0 {
		t.Errorf("foreman_limited events = %d, want 0", got)
	}
}

// TestLimitedWakeLeavesTheInboxAndExpiresLazily: wakeOutcomeTx returns
// limited and leaves the inbox pending; once until passes, the next wake
// clears it and goes through.
func TestLimitedWakeLeavesTheInboxAndExpiresLazily(t *testing.T) {
	e := newEnv(t)
	until := e.clock.Now().Add(time.Hour)
	e.limitForeman(until.Format(time.RFC3339))

	var out WakeOutcome
	wake := func() WakeOutcome {
		t.Helper()
		if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
			var err error
			out, err = e.eng.wakeOutcomeTx(tx, e.project, e.clock.Now())
			return err
		}); err != nil {
			t.Fatalf("wake: %v", err)
		}
		return out
	}
	if got := wake(); got != WakeLimited {
		t.Fatalf("wake outcome = %s, want %s", got, WakeLimited)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns = %d, want 1", got)
	}

	e.clock.Advance(2 * time.Hour)
	if got := wake(); got != WakeWoken {
		t.Fatalf("wake outcome after the reset = %s, want %s", got, WakeWoken)
	}
	if got := e.limitedState(); got != "" {
		t.Errorf("limited state after the reset = %q, want it cleared", got)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns after the reset = %d, want 2", got)
	}
}

// TestRecoverForemanWakesTakesTheLazyExpiry: the start-of-engine
// recovery skips a limited foreman and wakes one whose limit lapsed.
func TestRecoverForemanWakesTakesTheLazyExpiry(t *testing.T) {
	e := newEnv(t)
	e.limitForeman(e.clock.Now().Add(time.Hour).Format(time.RFC3339))
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Fatalf("foreman turns while limited = %d, want 1", got)
	}
	e.clock.Advance(2 * time.Hour)
	if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
		t.Fatalf("recover after the reset: %v", err)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns after the reset = %d, want 2", got)
	}
}

// TestAskNamesTheLimit: ask's result carries the limited outcome and the
// reset time, empty for unknown.
func TestAskNamesTheLimit(t *testing.T) {
	e := newEnv(t)
	until := e.clock.Now().Add(time.Hour)
	e.limitForeman(until.Format(time.RFC3339))
	res, err := e.eng.AskForeman(e.ctx, "", "status?", "cli")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if res.Outcome != WakeLimited || res.LimitedUntil != fmtTime(until) {
		t.Errorf("ask = %+v, want limited until %s", res, fmtTime(until))
	}

	e.exec("UPDATE projects SET foreman_limited = ? WHERE id = ?", foremanLimitUnknown, e.project)
	res, err = e.eng.AskForeman(e.ctx, "", "status again?", "cli")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if res.Outcome != WakeLimited || res.LimitedUntil != "" {
		t.Errorf("ask = %+v, want limited with no reset time", res)
	}
}

// TestStatusNeedsYouListsTheLimitedForeman: the limited project shows in
// the needs-you view with its reason, until it expires.
func TestStatusNeedsYouListsTheLimitedForeman(t *testing.T) {
	e := newEnv(t)
	e.limitForeman("")
	st, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var found bool
	for _, n := range st.NeedsYou {
		if n.Kind == "foreman" && n.ID == e.project && n.State == "limited" && strings.Contains(n.Reason, "reset time unknown") {
			found = true
		}
	}
	if !found {
		t.Errorf("needs you = %+v, want the limited foreman with its reason", st.NeedsYou)
	}
}

// TestSwitchingTheForemanClearsTheLimit: a model switch clears the
// limited state and wakes the held events; the session survives it.
func TestSwitchingTheModelClearsTheLimit(t *testing.T) {
	e := newEnv(t)
	e.limitForeman("")
	if _, err := e.eng.SetProjectForeman(e.ctx, "https://example.invalid/brigada.git", "claude", "opus"); err != nil {
		t.Fatalf("set foreman: %v", err)
	}
	if got := e.limitedState(); got != "" {
		t.Errorf("limited state after a model switch = %q, want cleared", got)
	}
	if got := e.foremanJobs(); got != 2 {
		t.Errorf("foreman turns after the switch = %d, want 2: the held events wake", got)
	}
}

// TestSameForemanSettingKeepsTheLimit: re-stating the same harness and
// model is no switch.
func TestSameForemanSettingKeepsTheLimit(t *testing.T) {
	e := newEnv(t)
	e.limitForeman("")
	if _, err := e.eng.SetProjectForeman(e.ctx, "https://example.invalid/brigada.git", "claude", "sonnet"); err != nil {
		t.Fatalf("set foreman: %v", err)
	}
	if got := e.limitedState(); got != foremanLimitUnknown {
		t.Errorf("limited state = %q, want it kept", got)
	}
}

// TestHarnessSwitchNeverResumesTheOldSession: the foreman key's session
// id belongs to the old harness's CLI; after a harness switch the next
// turn starts fresh, while a model switch on the same harness resumes.
func TestHarnessSwitchNeverResumesTheOldSession(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	g := e.leaseForeman()
	e.complete(g, Completion{Result: "judged", SessionID: "claude-sess-1"})

	if _, err := e.eng.SetProjectForeman(e.ctx, "https://example.invalid/brigada.git", "claude", "opus"); err != nil {
		t.Fatalf("switch the model: %v", err)
	}
	e.park("again?")
	g = e.leaseForeman()
	if g.ResumeSessionID != "claude-sess-1" {
		t.Fatalf("resume after a model switch = %q, want the session kept", g.ResumeSessionID)
	}
	e.complete(g, Completion{Result: "judged", SessionID: "claude-sess-2"})

	if _, err := e.eng.SetProjectForeman(e.ctx, "https://example.invalid/brigada.git", "fake", "scripted"); err != nil {
		t.Fatalf("switch the harness: %v", err)
	}
	e.park("a third?")
	g = e.lease(e.runner1, []string{"fake"}, nil)
	if g == nil {
		t.Fatal("the foreman turn was not leased on the new harness")
	}
	if g.ResumeSessionID != "" {
		t.Errorf("resume after a harness switch = %q, want a fresh session", g.ResumeSessionID)
	}
}
