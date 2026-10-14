package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"
)

// attach takes the fixture project's foreman, failing the test on the
// engine's refusal.
func (e *env) attach(force bool) AttachResult {
	e.t.Helper()
	res, err := e.eng.Attach(e.ctx, e.project, force)
	if err != nil {
		e.t.Fatalf("attach to project %d: %v", e.project, err)
	}
	return res
}

// attachHeartbeat beats one hold, failing the test on the engine's error.
func (e *env) attachHeartbeat(holdID int64) (Answer, time.Time) {
	e.t.Helper()
	ans, expires, err := e.eng.AttachHeartbeat(e.ctx, holdID)
	if err != nil {
		e.t.Fatalf("heartbeat attach hold %d: %v", holdID, err)
	}
	return ans, expires
}

// releaseAttach ends one hold, failing the test on the engine's error.
func (e *env) releaseAttach(holdID int64, sessionID string) Answer {
	e.t.Helper()
	ans, err := e.eng.ReleaseAttach(e.ctx, holdID, sessionID)
	if err != nil {
		e.t.Fatalf("release attach hold %d: %v", holdID, err)
	}
	return ans
}

// holdOf reads one attach hold's state and expiry.
func (e *env) holdOf(holdID int64) (state, expires string) {
	e.t.Helper()
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT state, expires_at FROM attach_holds WHERE id = ?", holdID).
			Scan(&state, &expires)
	}); err != nil {
		e.t.Fatalf("read attach hold %d: %v", holdID, err)
	}
	return state, expires
}

// inboxHolds reads the project's inbox rows as state[:hold] strings, in
// event order: what carried a row, when no turn did.
func (e *env) inboxHolds() []string {
	e.t.Helper()
	var out []string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT state || ':' || COALESCE(hold_id, 0) FROM foreman_inbox WHERE project_id = ? ORDER BY event_id", e.project)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatalf("read the foreman inbox: %v", err)
	}
	return out
}

// holdScopeOf resolves a secret through the engine's hold-token
// authentication path.
func (e *env) holdScopeOf(secret string) (project int64, ok bool) {
	e.t.Helper()
	p, _, ok, err := e.eng.HoldTokenScope(e.ctx, secret)
	if err != nil {
		e.t.Fatalf("resolve the hold token: %v", err)
	}
	return p, ok
}

// holdTokenHash reads the stored hash of a hold's token, "" when the
// hold holds none.
func (e *env) holdTokenHash(holdID int64) string {
	e.t.Helper()
	var h string
	err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT token_hash FROM hold_tokens WHERE hold_id = ?", holdID).Scan(&h)
	})
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		e.t.Fatalf("read the hold token of hold %d: %v", holdID, err)
	}
	return h
}

// parkPending parks the fixture's task and cancels the turn the park
// woke, leaving the event pending the way a cancelled turn leaves it —
// the state an attach's opening prompt takes.
func (e *env) parkPending(question string) {
	e.t.Helper()
	e.park(question)
	e.cancel(e.int("SELECT id FROM jobs WHERE kind = 'foreman'"))
}

// TestAttachTakesPendingEventsAndReleaseConsumes covers the hold's happy
// path (§4.2 item 1): attach answers created with the hold's id, the
// hold-scoped token, the foreman's harness and model, the key's empty
// session id, and an opening prompt carrying the pending event exactly
// as a wake prompt would; the row reads delivered to the hold. Release
// consumes the delivered rows, writes the session id back to the key's
// sessions row, and revokes the token.
func TestAttachTakesPendingEventsAndReleaseConsumes(t *testing.T) {
	e := newEnv(t)
	e.parkPending("which file?")
	res := e.attach(false)
	if res.Answer != AnsCreated {
		t.Fatalf("attach = %s, want created", res.Answer)
	}
	if res.HoldID == 0 {
		t.Fatal("attach named no hold")
	}
	if res.ForemanToken == "" || len(res.ForemanToken) != 2*leaseTokenEntropy {
		t.Fatalf("the hold token = %q, want %d hex chars", res.ForemanToken, 2*leaseTokenEntropy)
	}
	digest := sha256.Sum256([]byte(res.ForemanToken))
	if h := e.holdTokenHash(res.HoldID); h != hex.EncodeToString(digest[:]) {
		t.Errorf("stored hash = %s, want sha256 of the secret", h)
	}
	if res.Harness != "claude" || res.Model != "sonnet" {
		t.Errorf("the foreman = %s/%s, want claude/sonnet", res.Harness, res.Model)
	}
	if res.SessionID != "" {
		t.Errorf("the key's session id = %q, want empty before any run", res.SessionID)
	}
	if want := attachHoldTTL; res.ExpiresAt.Sub(e.clock.Now()) != want {
		t.Errorf("the hold expires in %s, want %s", res.ExpiresAt.Sub(e.clock.Now()), want)
	}
	want := "<event>\n" + e.wakeLine(evTaskNeedsHelp, "which file?") + "\n</event>"
	if !strings.Contains(res.Prompt, want) {
		t.Errorf("the opening prompt = %q, want it to carry the event as a wake prompt would:\n%s", res.Prompt, want)
	}
	if !strings.Contains(res.Prompt, "You are waking up for one headless turn") {
		t.Errorf("the opening prompt = %q, want the wake template in it", res.Prompt)
	}
	if want := []string{"delivered:" + fmt.Sprint(res.HoldID)}; !equalStrings(e.inboxHolds(), want) {
		t.Errorf("the inbox after the attach = %v, want the row delivered to the hold", e.inboxHolds())
	}
	if p, ok := e.holdScopeOf(res.ForemanToken); !ok || p != e.project {
		t.Errorf("the hold token's scope = project %d, %v; want %d, true", p, ok, e.project)
	}
	if p, ok := e.eventPayloadOf(evForemanAttached); !ok || p.Hold != res.HoldID || p.Count != 1 {
		t.Errorf("foreman_attached payload = %+v, want the hold named, one event counted", p)
	}

	if ans := e.releaseAttach(res.HoldID, "fs-42"); ans != AnsOK {
		t.Fatalf("release = %s, want ok", ans)
	}
	if want := []string{"consumed:" + fmt.Sprint(res.HoldID)}; !equalStrings(e.inboxHolds(), want) {
		t.Errorf("the inbox after the release = %v, want the row consumed", e.inboxHolds())
	}
	if got := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", fmt.Sprintf("foreman:%d", e.project)); got != "fs-42" {
		t.Errorf("the key's session id after the release = %q, want fs-42", got)
	}
	if h := e.holdTokenHash(res.HoldID); h != "" {
		t.Errorf("the hold's token survived the release: %s", h)
	}
	if _, ok := e.holdScopeOf(res.ForemanToken); ok {
		t.Error("the hold token still resolves after the release")
	}
	if state, _ := e.holdOf(res.HoldID); state != "released" {
		t.Errorf("the hold = %s, want released", state)
	}
	if _, ok := e.eventPayloadOf(evForemanDetached); !ok {
		t.Error("no foreman_detached event")
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after the release = %d, want 1: nothing is pending, so no wake", got)
	}
}

// TestSecondAttachIsRefused is the one-live-hold rule (§4.2 item 1): a
// second attach answers busy with the live hold, and a released hold
// frees the foreman for the next attach.
func TestSecondAttachIsRefused(t *testing.T) {
	e := newEnv(t)
	res := e.attach(false)
	again := e.attach(false)
	if again.Answer != AnsBusy {
		t.Fatalf("the second attach = %s, want busy", again.Answer)
	}
	if again.HoldID != res.HoldID || again.ForemanToken != "" {
		t.Errorf("the busy answer = %+v, want the live hold named and no token", again)
	}
	e.releaseAttach(res.HoldID, "")
	next := e.attach(false)
	if next.Answer != AnsCreated || next.HoldID == res.HoldID {
		t.Errorf("the attach after the release = %+v, want a fresh hold", next)
	}
}

// TestAttachBusyTurnAndForce is the turn rule (§4.2 item 1): a live
// foreman turn makes attach busy with the turn's id, and force cancels
// it through the cancel path — its events return to pending uncounted
// and the opening prompt takes them, so nothing a worker asked is lost.
func TestAttachBusyTurnAndForce(t *testing.T) {
	e := newEnv(t)
	e.park("which file?")
	turn := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
	busy := e.attach(false)
	if busy.Answer != AnsBusy || busy.JobID != turn {
		t.Fatalf("attach with a live turn = %+v, want busy naming turn %d", busy, turn)
	}
	forced := e.attach(true)
	if forced.Answer != AnsCreated {
		t.Fatalf("the forced attach = %s, want created", forced.Answer)
	}
	if status := e.str("SELECT status FROM jobs WHERE id = ?", turn); status != statusCancelled {
		t.Errorf("the live turn = %s, want cancelled", status)
	}
	if reason := e.str("SELECT json_extract(result, '$.error') FROM jobs WHERE id = ?", turn); reason != "" {
		t.Errorf("the cancelled turn carries an error %q, want none", reason)
	}
	if got := e.int("SELECT attempts FROM foreman_inbox"); got != 0 {
		t.Errorf("attempts after the forced attach = %d, want 0: a cancel never counts", got)
	}
	if want := []string{"delivered:" + fmt.Sprint(forced.HoldID)}; !equalStrings(e.inboxHolds(), want) {
		t.Errorf("the inbox after the forced attach = %v, want the cancelled turn's event delivered to the hold", e.inboxHolds())
	}
	want := "<event>\n" + e.wakeLine(evTaskNeedsHelp, "which file?") + "\n</event>"
	if !strings.Contains(forced.Prompt, want) {
		t.Errorf("the opening prompt = %q, want the cancelled turn's event in it:\n%s", forced.Prompt, want)
	}
	if got := e.foremanJobs(); got != 1 {
		t.Errorf("foreman turns after the forced attach = %d, want 1: the cancelled one, no re-wake", got)
	}
}

// TestParkWhileAttachedWaitsForTheSession is the attached rule (§4.2
// item 1): a park while a hold lives submits no turn, and the attached
// foreman reads the event through ForemanEvents — which renders it as
// the wake prompt would and consumes it, so a second read says nothing
// new.
func TestParkWhileAttachedWaitsForTheSession(t *testing.T) {
	e := newEnv(t)
	res := e.attach(false)
	e.park("which file?")
	if got := e.foremanJobs(); got != 0 {
		t.Fatalf("foreman turns while the hold lives = %d, want 0: no wake past an attach", got)
	}
	events, err := e.eng.ForemanEvents(e.ctx, e.project)
	if err != nil {
		t.Fatalf("read the foreman events: %v", err)
	}
	if want := []string{e.wakeLine(evTaskNeedsHelp, "which file?")}; !equalStrings(events.Events, want) || len(events.UserMessages) != 0 {
		t.Fatalf("the events = %+v, want the park's line verbatim and no user message", events)
	}
	// The row arrived while the hold lived, so it was never delivered to
	// the hold — the read consumes it from pending, holdless.
	if want := []string{"consumed:0"}; !equalStrings(e.inboxHolds(), want) {
		t.Errorf("the inbox after the read = %v, want the row consumed", e.inboxHolds())
	}
	again, err := e.eng.ForemanEvents(e.ctx, e.project)
	if err != nil {
		t.Fatalf("read the foreman events again: %v", err)
	}
	if !again.Empty() {
		t.Errorf("the second read = %+v, want empty: the events are consumed", again)
	}
	e.releaseAttach(res.HoldID, "")
	if got := e.foremanJobs(); got != 0 {
		t.Errorf("foreman turns after the release = %d, want 0: nothing pending, nothing woken", got)
	}
}

// TestAttachExpiryReturnsTheRowsAndWakes is the sweep's hold round
// (§4.2 item 1): a hold that lapses unheartbeated expires — its
// delivered rows return to pending without counting an attempt, its
// token is revoked, one foreman_attach_expired event names it, and the
// wake it deferred is re-evaluated, turning the events over to a
// headless turn.
func TestAttachExpiryReturnsTheRowsAndWakes(t *testing.T) {
	t.Run("returns the rows and revokes the token", func(t *testing.T) {
		e := newEnv(t)
		e.parkPending("which file?")
		res := e.attach(false)
		// No harness: the expiry's re-evaluation wakes nothing, so the
		// returned row is observable sitting pending.
		e.exec("UPDATE projects SET foreman_harness = '' WHERE id = ?", e.project)
		e.clock.Advance(attachHoldTTL + time.Second)
		e.mustSweep()
		if state, _ := e.holdOf(res.HoldID); state != "expired" {
			t.Errorf("the lapsed hold = %s, want expired", state)
		}
		if h := e.holdTokenHash(res.HoldID); h != "" {
			t.Errorf("the lapsed hold's token survived: %s", h)
		}
		if _, ok := e.holdScopeOf(res.ForemanToken); ok {
			t.Error("the lapsed hold's token still resolves")
		}
		// The row keeps its hold, as a returned row keeps its job (0013).
		if want := []string{"pending:" + fmt.Sprint(res.HoldID)}; !equalStrings(e.inboxHolds(), want) {
			t.Errorf("the inbox after the expiry = %v, want the row pending again", e.inboxHolds())
		}
		if got := e.int("SELECT attempts FROM foreman_inbox"); got != 0 {
			t.Errorf("attempts after the expiry = %d, want 0: a dead terminal is not a failed answer", got)
		}
		if p, ok := e.eventPayloadOf(evForemanAttachExpired); !ok || p.Hold != res.HoldID {
			t.Errorf("foreman_attach_expired payload = %+v, want the hold named", p)
		}
		if got := e.foremanJobs(); got != 1 {
			t.Errorf("foreman turns after the expiry = %d, want 1: the cancelled one alone, no wake past no harness", got)
		}
	})

	t.Run("then wakes", func(t *testing.T) {
		e := newEnv(t)
		e.parkPending("which file?")
		res := e.attach(false)
		e.clock.Advance(attachHoldTTL + time.Second)
		e.mustSweep()
		if got := e.foremanJobs(); got != 2 {
			t.Fatalf("foreman turns after the expiry = %d, want 2: the wake the hold deferred", got)
		}
		job := e.int("SELECT id FROM jobs WHERE kind = 'foreman' AND status = 'queued'")
		if want := []string{"delivered:" + fmt.Sprint(job)}; !equalStrings(e.inboxStates(), want) {
			t.Errorf("the inbox after the wake = %v, want the row delivered to the new turn", e.inboxStates())
		}
		if state, _ := e.holdOf(res.HoldID); state != "expired" {
			t.Errorf("the lapsed hold = %s, want expired", state)
		}
		want := "<event>\n" + e.wakeLine(evTaskNeedsHelp, "which file?") + "\n</event>"
		if prompt := e.str("SELECT prompt FROM jobs WHERE id = ?", job); !strings.Contains(prompt, want) {
			t.Errorf("the woken turn's prompt = %q, want the returned event in it:\n%s", prompt, want)
		}
	})
}

// TestAttachTokenDiesAtExpiryWithoutSweep covers the quiet-engine rule:
// the expiry is checked at authentication time, so a lapsed hold's token
// is refused even before the sweep has expired the row.
func TestAttachTokenDiesAtExpiryWithoutSweep(t *testing.T) {
	e := newEnv(t)
	res := e.attach(false)
	e.clock.Advance(attachHoldTTL + time.Second)
	if _, ok := e.holdScopeOf(res.ForemanToken); ok {
		t.Error("the token resolved past its expiry, sweep or no sweep")
	}
	if h := e.holdTokenHash(res.HoldID); h == "" {
		t.Fatal("the row vanished without a sweep; the refusal should have been the expiry check's")
	}
}

// TestAttachHeartbeatExtendsTheHold is the hold's lease-like life
// (§4.2 item 1): a beat moves the hold's expiry and its token's out
// together, so the attached session works past the original expiry but
// not past the extended one; a beat on a released hold is gone.
func TestAttachHeartbeatExtendsTheHold(t *testing.T) {
	e := newEnv(t)
	res := e.attach(false)
	e.clock.Advance(30 * time.Second)
	ans, expires := e.attachHeartbeat(res.HoldID)
	if ans != AnsOK {
		t.Fatalf("heartbeat = %s, want ok", ans)
	}
	if want := e.clock.Now().Add(attachHoldTTL); !expires.Equal(want) {
		t.Errorf("the hold's new expiry = %s, want %s", expires, want)
	}
	if state, at := e.holdOf(res.HoldID); state != "live" || at != fmtTime(expires) {
		t.Errorf("the hold = %s at %s, want live at %s", state, at, fmtTime(expires))
	}
	if h := e.holdTokenHash(res.HoldID); h == "" {
		t.Fatal("the heartbeat lost the hold's token")
	}

	// Past the original expiry, before the extended one: still good.
	e.clock.Advance(attachHoldTTL - time.Second)
	if _, ok := e.holdScopeOf(res.ForemanToken); !ok {
		t.Error("the token died before its extended expiry")
	}
	if ans, _ := e.attachHeartbeat(res.HoldID); ans != AnsOK {
		t.Errorf("heartbeat past the original expiry = %s, want ok", ans)
	}

	e.releaseAttach(res.HoldID, "")
	if ans, _ := e.attachHeartbeat(res.HoldID); ans != AnsGone {
		t.Errorf("heartbeat after the release = %s, want gone", ans)
	}
}

// TestForcedAttachSurvivesWakeRecovery is the heal's boundary (§4.2 item
// 1, §13): a forced attach cancels the live turn and takes its rows into
// the hold, and the start-of-engine recovery — whose heal returns a
// delivered row whose job is no longer live — must leave them delivered,
// or a restart while a hold lives would hand the attached session's
// opening prompt to a second turn and the events tool would repeat it.
func TestForcedAttachSurvivesWakeRecovery(t *testing.T) {
	t.Run("the forced attach's rows keep no stale job", func(t *testing.T) {
		e := newEnv(t)
		e.park("which file?")
		turn := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
		res := e.attach(true)
		if res.Answer != AnsCreated {
			t.Fatalf("the forced attach = %s, want created", res.Answer)
		}
		if status := e.str("SELECT status FROM jobs WHERE id = ?", turn); status != statusCancelled {
			t.Fatalf("the live turn = %s, want cancelled by the forced attach", status)
		}
		if got := e.int("SELECT COUNT(*) FROM foreman_inbox WHERE job_id IS NOT NULL"); got != 0 {
			t.Fatalf("rows delivered to the hold still name a job: %d do", got)
		}
		if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
			t.Fatalf("recover the wakes: %v", err)
		}
		if want := []string{"delivered:" + fmt.Sprint(res.HoldID)}; !equalStrings(e.inboxHolds(), want) {
			t.Errorf("the inbox after the recovery = %v, want the row still delivered to the hold", e.inboxHolds())
		}
		if got := e.foremanJobs(); got != 1 {
			t.Errorf("foreman turns after the recovery = %d, want 1: the cancelled one, no second carry", got)
		}
	})

	t.Run("the heal leaves a live hold's rows alone", func(t *testing.T) {
		// A row written before the job_id clear may sit delivered to a
		// live hold with the cancelled turn's id still on it; the heal's
		// own rule — not the NULL — must keep it.
		e := newEnv(t)
		e.parkPending("which file?")
		res := e.attach(false)
		e.exec("UPDATE foreman_inbox SET job_id = ? WHERE hold_id = ?",
			e.int("SELECT id FROM jobs WHERE kind = 'foreman'"), res.HoldID)
		if err := e.eng.RecoverForemanWakes(e.ctx); err != nil {
			t.Fatalf("recover the wakes: %v", err)
		}
		if want := []string{"delivered:" + fmt.Sprint(res.HoldID)}; !equalStrings(e.inboxHolds(), want) {
			t.Errorf("the inbox after the recovery = %v, want the row still delivered to the hold", e.inboxHolds())
		}
	})
}

// TestReleaseAfterExpiryKeepsTheSessionID is the late release (§4.2 item
// 1): a release that arrives after the sweep expired the hold — the
// laptop slept through the expiry — still writes the session id the chat
// ended on, for the next wake must resume the id the chat reached, and
// answers expired so the client can say the hold lapsed. The expiry has
// already returned the rows, revoked the token and re-evaluated the
// wake; the late release consumes nothing and logs no detach.
func TestReleaseAfterExpiryKeepsTheSessionID(t *testing.T) {
	e := newEnv(t)
	e.parkPending("which file?")
	res := e.attach(false)
	e.clock.Advance(attachHoldTTL + time.Second)
	ans := e.releaseAttach(res.HoldID, "fs-9")
	if ans != AnsExpired {
		t.Fatalf("the late release = %s, want expired", ans)
	}
	if got := e.str("SELECT harness_session_id FROM sessions WHERE key = ?",
		fmt.Sprintf("foreman:%d", e.project)); got != "fs-9" {
		t.Errorf("the key's session id after the late release = %q, want fs-9", got)
	}
	// The expiry's wake took the rows into a fresh turn; the late release
	// consumes nothing on top of that.
	if got := e.int("SELECT COUNT(*) FROM foreman_inbox WHERE state = 'consumed'"); got != 0 {
		t.Errorf("consumed rows after the late release = %d, want 0: the expiry owns the rows", got)
	}
	if got := e.int("SELECT attempts FROM foreman_inbox"); got != 0 {
		t.Errorf("attempts after the late release = %d, want 0", got)
	}
	if got := e.countEvents(evForemanDetached); got != 0 {
		t.Errorf("foreman_detached events after the late release = %d, want 0: nothing live detached", got)
	}
	if h := e.holdTokenHash(res.HoldID); h != "" {
		t.Errorf("the lapsed hold's token survived the late release: %s", h)
	}
	if ans := e.releaseAttach(res.HoldID, ""); ans != AnsExpired {
		t.Errorf("a repeated late release = %s, want expired again", ans)
	}
}

// TestReleaseKeepsTheRunnerAffinity is the release's write-back rule
// (§4.2, §8): the attached chat ran in the user's own terminal, no
// engine runner, so its release writes the session id alone and keeps
// the runner the key's last lease pinned — the release must not free
// the key to whoever leases next.
func TestReleaseKeepsTheRunnerAffinity(t *testing.T) {
	e := newEnv(t)
	key := fmt.Sprintf("foreman:%d", e.project)
	e.exec(`INSERT INTO sessions (key, runner_id, harness_session_id, state, last_used_at, created_at)
		VALUES (?, ?, 'fs-old', 'active', ?, ?)`, key, e.runner2, seedTS, seedTS)
	res := e.attach(false)
	e.releaseAttach(res.HoldID, "fs-9")
	if got := e.int("SELECT COALESCE(runner_id, 0) FROM sessions WHERE key = ?", key); got != e.runner2 {
		t.Errorf("the key's runner after the release = %d, want %d kept", got, e.runner2)
	}
	if got := e.str("SELECT harness_session_id FROM sessions WHERE key = ?", key); got != "fs-9" {
		t.Errorf("the key's session id after the release = %q, want fs-9", got)
	}
}

// TestLiveAttachesCountsLiveHolds is the idle rule's "attached
// sessions" (§13): a heartbeated hold is an open chat, and the engine
// of an attach that is still beating counts as busy however quiet the
// chat; the release and the lapse both end that.
func TestLiveAttachesCountsLiveHolds(t *testing.T) {
	e := newEnv(t)
	if n, err := e.eng.LiveAttaches(e.ctx); err != nil || n != 0 {
		t.Fatalf("live attaches with none = %d (%v), want 0", n, err)
	}
	res := e.attach(false)
	if n, err := e.eng.LiveAttaches(e.ctx); err != nil || n != 1 {
		t.Fatalf("live attaches with a hold = %d (%v), want 1", n, err)
	}
	e.attachHeartbeat(res.HoldID)
	if n, err := e.eng.LiveAttaches(e.ctx); err != nil || n != 1 {
		t.Fatalf("live attaches after a beat = %d (%v), want 1: a beating hold is a live session", n, err)
	}
	e.releaseAttach(res.HoldID, "fs-1")
	if n, err := e.eng.LiveAttaches(e.ctx); err != nil || n != 0 {
		t.Fatalf("live attaches after the release = %d (%v), want 0", n, err)
	}
	// A hold that outlived its expiry is no session still attached: the
	// sweep settles it, and the idle rule does not wait on it.
	res = e.attach(false)
	e.clock.Advance(2 * attachHoldTTL)
	if n, err := e.eng.LiveAttaches(e.ctx); err != nil || n != 0 {
		t.Fatalf("live attaches past the expiry = %d (%v), want 0", n, err)
	}
}
