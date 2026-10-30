package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

// usage is a completion's reported usage: input and cached input tokens.
func usage(in, cached int64) *adapter.Usage {
	return &adapter.Usage{InputTokens: &in, CachedInputTokens: &cached}
}

// foremanTurnDone wakes, leases and completes one foreman turn of the
// fixture project, returning the prompt it ran with.
func (e *env) foremanTurnDone(sessionID string, u *adapter.Usage) string {
	e.t.Helper()
	r, err := e.eng.SubmitForemanTurn(e.ctx, e.project, nil)
	if err != nil || r.Answer != AnsCreated {
		e.t.Fatalf("submit foreman turn = %+v, %v; want created", r, err)
	}
	g := e.leaseClaude(e.runner1)
	if g == nil || g.JobID != r.JobID {
		e.t.Fatalf("the lease did not take the turn %d: %+v", r.JobID, g)
	}
	c := done()
	c.SessionID, c.Usage = sessionID, u
	if ans := e.complete(g, c); ans != AnsOK {
		e.t.Fatalf("complete the turn: %s", ans)
	}
	return e.str("SELECT prompt FROM jobs WHERE id = ?", r.JobID)
}

func (e *env) rotations() int64 {
	e.t.Helper()
	return e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evForemanSessionRotated)
}

func (e *env) foremanSession() string {
	e.t.Helper()
	return e.str("SELECT harness_session_id FROM sessions WHERE key = ?", fmt.Sprintf("foreman:%d", e.project))
}

// TestForemanRotationCrossesOnce covers §4.2's rotation: a turn that read
// more than foreman_rotate_tokens (input plus cached) makes the next turn
// start a fresh session, once — the generation moves, the session id
// clears, the event says why — and that first turn alone carries the
// digest.
func TestForemanRotationCrossesOnce(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)

	first := e.foremanTurnDone("s1", usage(450000, 300000))
	if strings.Contains(first, "<digest>") {
		t.Error("the project's first turn carries a digest; only a rotated session's does")
	}
	if got := e.foremanSession(); got != "s1" {
		t.Fatalf("session = %q, want s1", got)
	}

	second := e.foremanTurnDone("s2", usage(1000, 0))
	if n := e.rotations(); n != 1 {
		t.Fatalf("rotations = %d, want 1", n)
	}
	want := fmt.Sprintf(`{"tokens":450000,"threshold":%d,"gen":1}`, defaultForemanRotateTokens)
	if got := e.str("SELECT payload FROM events WHERE kind = ?", evForemanSessionRotated); got != want {
		t.Errorf("rotation payload = %s, want %s", got, want)
	}
	if gen := e.int("SELECT foreman_session_gen FROM projects WHERE id = ?", e.project); gen != 1 {
		t.Errorf("foreman_session_gen = %d, want 1", gen)
	}
	if !strings.Contains(second, "<digest>") || !strings.Contains(second, "The previous session was rotated: its last turn read 450000 input tokens") {
		t.Errorf("the rotated session's first turn has no digest:\n%s", second)
	}
	if strings.Index(second, "</digest>") < strings.Index(second, "<digest>") {
		t.Error("the digest is not closed")
	}
	// The turn ran fresh and wrote its own session back.
	if got := e.foremanSession(); got != "s2" {
		t.Errorf("session = %q, want s2", got)
	}

	third := e.foremanTurnDone("s3", usage(1000, 0))
	if n := e.rotations(); n != 1 {
		t.Errorf("rotations = %d, want still 1: the crossing turn belongs to the old generation", n)
	}
	if strings.Contains(third, "<digest>") {
		t.Error("a later turn of the generation repeats the digest")
	}
}

// TestForemanRotationUnknownUsage: a turn whose adapter reported no
// usage never rotates the session.
func TestForemanRotationUnknownUsage(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	e.foremanTurnDone("s1", nil)
	e.foremanTurnDone("s2", nil)
	if n := e.rotations(); n != 0 {
		t.Errorf("rotations = %d, want 0 on unknown usage", n)
	}
	if got := e.foremanSession(); got != "s2" {
		t.Errorf("session = %q, want s2 kept", got)
	}
	// Cached input is a breakdown of the input count (adapter.Usage), not
	// added to it: 390k input of which 380k cached stays under 400k.
	e.foremanTurnDone("s3", usage(390000, 380000))
	e.foremanTurnDone("s4", nil)
	if n := e.rotations(); n != 0 {
		t.Errorf("rotations = %d, want 0: cached input counted twice", n)
	}
	// Only the cached count known: it stands for the input.
	e.exec("UPDATE jobs SET input_tokens = NULL, cached_input_tokens = 500000 WHERE id = (SELECT MAX(id) FROM jobs)")
	e.foremanTurnDone("s5", nil)
	if n := e.rotations(); n != 1 {
		t.Errorf("rotations = %d, want 1 on a known cached count alone", n)
	}
}

// TestForemanRotationDisabled: foreman_rotate_tokens 0 never rotates, and
// a threshold set below a turn's usage does.
func TestForemanRotationDisabled(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	e.set(map[string]string{SettingForemanRotateTokens: "0"})
	e.foremanTurnDone("s1", usage(5_000_000, 0))
	e.foremanTurnDone("s2", usage(5_000_000, 0))
	if n := e.rotations(); n != 0 {
		t.Fatalf("rotations = %d, want 0 with the setting at 0", n)
	}
	e.set(map[string]string{SettingForemanRotateTokens: "1000"})
	e.foremanTurnDone("s3", usage(10, 0))
	if n := e.rotations(); n != 1 {
		t.Errorf("rotations = %d, want 1 once the threshold is under the usage", n)
	}
}

// TestForemanRotationBlocked: never while a turn is live or a chat is
// attached.
func TestForemanRotationBlocked(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	e.foremanTurnDone("s1", usage(10, 0))
	first := e.int("SELECT MAX(id) FROM jobs WHERE kind = ?", KindForeman)

	// A live turn: a wake behind it coalesces and rotates nothing.
	r, err := e.eng.SubmitForemanTurn(e.ctx, e.project, nil)
	if err != nil || r.Answer != AnsCreated {
		t.Fatalf("submit = %+v, %v", r, err)
	}
	g := e.leaseClaude(e.runner1)
	e.exec("UPDATE jobs SET input_tokens = 900000 WHERE id = ?", first)
	if _, err := e.eng.SubmitForemanTurn(e.ctx, e.project, nil); err != nil {
		t.Fatal(err)
	}
	if res := e.attach(false); res.Answer != AnsBusy {
		t.Fatalf("attach behind a live turn = %s, want busy", res.Answer)
	}
	if n := e.rotations(); n != 0 {
		t.Fatalf("rotations = %d while a turn is live, want 0", n)
	}
	c := done()
	c.SessionID = "s2"
	if ans := e.complete(g, c); ans != AnsOK {
		t.Fatalf("complete: %s", ans)
	}

	// An attached chat: nothing rotates under it.
	e.exec("UPDATE jobs SET input_tokens = 900000 WHERE id = ?", g.JobID)
	hold := e.attach(false)
	if hold.Answer != AnsCreated {
		t.Fatalf("attach = %s, want created", hold.Answer)
	}
	if n := e.rotations(); n != 1 {
		t.Fatalf("rotations = %d, want the attach's own rotation", n)
	}
	if hold.SessionID != "" || !strings.Contains(hold.Prompt, "<digest>") {
		t.Errorf("a rotating attach resumes %q / prompt without digest; want a fresh session with the digest", hold.SessionID)
	}
	// The chat's own crossing: no rotation while its hold lives.
	e.exec("UPDATE jobs SET input_tokens = 900000, session_gen = 1 WHERE id = ?", g.JobID)
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		rotated, err := e.eng.rotateForemanSessionTx(tx, e.project, e.clock.Now())
		if rotated {
			t.Error("rotated under a live attach hold")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := e.rotations(); n != 1 {
		t.Errorf("rotations = %d while attached, want 1", n)
	}
}

// TestForemanRotationAttach: an attach after a rotation opens with the
// digest and resumes nothing; the next attach, once the chat wrote its
// session back, has no digest.
func TestForemanRotationAttach(t *testing.T) {
	e := newEnv(t)
	e.offerForeman(e.runner1)
	e.foremanTurnDone("s1", usage(500000, 0))
	res := e.attach(false)
	if res.Answer != AnsCreated || res.SessionID != "" || !strings.Contains(res.Prompt, "<digest>") {
		t.Fatalf("attach after crossing = %s, session %q; want created, fresh, with digest", res.Answer, res.SessionID)
	}
	if ans := e.releaseAttach(res.HoldID, "chat-1"); ans != AnsOK {
		t.Fatalf("release: %s", ans)
	}
	again := e.attach(false)
	if again.SessionID != "chat-1" || strings.Contains(again.Prompt, "<digest>") {
		t.Errorf("second attach resumes %q, digest %v; want chat-1 and no digest", again.SessionID, strings.Contains(again.Prompt, "<digest>"))
	}
	if n := e.rotations(); n != 1 {
		t.Errorf("rotations = %d, want 1", n)
	}
}

// TestForemanDigestBounded: many plans, tasks and long titles still make
// a digest under its byte bound, and the cut lists say what they left.
func TestForemanDigestBounded(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("a very long title ", 40)
	err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		for p := 0; p < 15; p++ {
			res, err := tx.Exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at) VALUES (?, ?, 'active', ?, ?)`,
				e.project, long, seedTS, seedTS)
			if err != nil {
				return err
			}
			plan, _ := res.LastInsertId()
			for k := 0; k < 30; k++ {
				if _, err := tx.Exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
					VALUES (?, ?, 'the spec', 'go test ./...', 'implement', 'needs_foreman', 'main', '{}', ?, ?)`,
					plan, long, seedTS, seedTS); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		digest, err = foremanDigestTx(tx, &e.eng.cfg, e.project, e.clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(digest) > digestMaxBytes {
		t.Errorf("digest is %d bytes, over %d", len(digest), digestMaxBytes)
	}
	for _, want := range []string{"Plans active, reviewing, parked or awaiting approval: 16", "more tasks omitted", "omitted"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest lacks %q:\n%s", want, digest)
		}
	}
	for _, line := range strings.Split(digest, "\n") {
		if len(line) > 2*digestLineLimit {
			t.Errorf("digest line of %d bytes: %q", len(line), line)
		}
	}
}

// TestForemanRotateSetting covers the setting's set, show and import.
func TestForemanRotateSetting(t *testing.T) {
	e := newEnv(t)
	show := func() SettingValue {
		t.Helper()
		got, err := e.eng.ProjectSettings(e.ctx, envRepo)
		if err != nil || got == nil {
			t.Fatalf("show: %+v %v", got, err)
		}
		return settingOf(t, *got, SettingForemanRotateTokens)
	}
	if s := show(); s.Value != "400000" || !s.Default {
		t.Errorf("default = %+v, want 400000 default", s)
	}
	e.set(map[string]string{SettingForemanRotateTokens: "0"})
	if s := show(); s.Value != "0" || s.Source != SourceLocal {
		t.Errorf("after set 0 = %+v, want 0 local", s)
	}
	for _, bad := range []string{"-1", "lots", "1.5"} {
		if _, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{SettingForemanRotateTokens: bad}); err == nil {
			t.Errorf("set %q succeeded; want refusal", bad)
		}
	}
	e.set(map[string]string{SettingForemanRotateTokens: ""})
	if s := show(); s.Value != "400000" || !s.Default {
		t.Errorf("after reset = %+v, want the default", s)
	}
	content := cfgFile("foreman_rotate_tokens: 250000\n")
	e.importCfg(content, false)
	if s := show(); s.Value != "250000" || s.Source != SourceImported {
		t.Errorf("after import = %+v, want 250000 imported", s)
	}
	var n int64
	if err := e.st.Update(e.ctx, func(tx *sql.Tx) error {
		var err error
		n, err = foremanRotateTokensTx(tx, e.project)
		return err
	}); err != nil || n != 250000 {
		t.Errorf("threshold read = %d, %v; want 250000", n, err)
	}
	if _, err := ParseProjectConfig(cfgFile("foreman_rotate_tokens: [1]\n")); err == nil {
		t.Error("a list value was accepted")
	}
}
