package engine

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// acquire takes the fixture project's slot for a bridge instance,
// failing the test on the engine's error.
func (e *env) acquire(bridge, label string) HoldResult {
	e.t.Helper()
	res, err := e.eng.AcquireHold(e.ctx, HoldAcquire{Project: e.project, BridgeInstance: bridge, Label: label})
	if err != nil {
		e.t.Fatalf("acquire a hold for %s: %v", bridge, err)
	}
	return res
}

// slot reads the fixture project's slot.
func (e *env) slot() Slot {
	e.t.Helper()
	s, err := e.eng.ForemanSlot(e.ctx, e.project)
	if err != nil {
		e.t.Fatalf("read the slot: %v", err)
	}
	return s
}

// holdRow reads one interactive hold's state and end reason.
func (e *env) holdRow(id int64) (state, reason string) {
	e.t.Helper()
	var r *string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT state, end_reason FROM interactive_holds WHERE id = ?", id).Scan(&state, &r)
	}); err != nil {
		e.t.Fatalf("read interactive hold %d: %v", id, err)
	}
	if r != nil {
		reason = *r
	}
	return state, reason
}

func (e *env) wantSlot(state SlotState) Slot {
	e.t.Helper()
	s := e.slot()
	if s.State != state {
		e.t.Fatalf("slot = %q, want %q", s.State, state)
	}
	return s
}

// ask puts a user message in the foreman's inbox, failing the test on
// the engine's error.
func (e *env) askForeman(text string) AskResult {
	e.t.Helper()
	res, err := e.eng.AskForeman(e.ctx, "", text, "alice")
	if err != nil {
		e.t.Fatalf("ask the foreman: %v", err)
	}
	return res
}

// staleReason asserts the call was rejected as a stale hold.
func staleReason(t *testing.T, err error) *StaleHoldError {
	t.Helper()
	var s *StaleHoldError
	if !errors.Is(err, ErrStaleHold) || !errors.As(err, &s) {
		t.Fatalf("err = %v, want a stale hold", err)
	}
	return s
}

// TestHoldTransitions walks §4.6's table: every way into and out of
// each slot state.
func TestHoldTransitions(t *testing.T) {
	t.Run("free to interactive hold and back by yield", func(t *testing.T) {
		e := newEnv(t)
		e.wantSlot(SlotFree)
		h := e.acquire("bridge-a", "my claude")
		if h.Answer != AnsCreated || h.Credential == "" || h.Fence == 0 {
			t.Fatalf("acquire = %+v, want created with a credential and a fence", h)
		}
		s := e.wantSlot(SlotInteractive)
		if s.HolderID != h.HoldID || s.Label != "my claude" || s.Since == "" {
			t.Errorf("slot = %+v, want hold %d labelled and dated", s, h.HoldID)
		}
		if err := e.eng.YieldHold(e.ctx, h.HoldID, h.Fence); err != nil {
			t.Fatalf("yield: %v", err)
		}
		e.wantSlot(SlotFree)
		if state, reason := e.holdRow(h.HoldID); state != "ended" || reason != HoldEndYield {
			t.Errorf("hold = %s/%s, want ended/yield", state, reason)
		}
	})

	t.Run("interactive hold to free by inactivity, max, lost lease, forced release", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			reason string
			end    func(e *env, h HoldResult)
		}{
			{"idle", HoldEndIdle, func(e *env, h HoldResult) {
				e.beatUntil(h, 16*time.Minute)
			}},
			{"max", HoldEndMax, func(e *env, h HoldResult) {
				e.keepActiveUntil(h, 241*time.Minute)
			}},
			{"lease", HoldEndLease, func(e *env, _ HoldResult) {
				e.clock.Advance(holdLeaseTTL + time.Second)
			}},
			{"forced", HoldEndForced, func(e *env, _ HoldResult) {
				if ans, err := e.eng.ReleaseHoldForced(e.ctx, e.project); err != nil || ans != AnsOK {
					e.t.Fatalf("forced release = %v, %v", ans, err)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := newEnv(t)
				h := e.acquire("bridge-a", "")
				tc.end(e, h)
				e.wantSlot(SlotFree)
				if _, reason := e.holdRow(h.HoldID); reason != tc.reason {
					t.Errorf("end reason = %q, want %q", reason, tc.reason)
				}
			})
		}
	})

	t.Run("free to headless turn by a wake, and back at turn end", func(t *testing.T) {
		e := newEnv(t)
		if r := e.askForeman("hello"); r.Outcome != WakeWoken {
			t.Fatalf("ask from a free slot = %s, want woken", r.Outcome)
		}
		s := e.wantSlot(SlotTurn)
		if s.HolderID == 0 || s.Since == "" || len(s.Events) != 1 {
			t.Errorf("turn slot = %+v, want the job, its start and its one event", s)
		}
		if ans, err := e.eng.Cancel(e.ctx, s.HolderID); err != nil || ans != AnsOK {
			t.Fatalf("cancel the turn = %v, %v", ans, err)
		}
		e.wantSlot(SlotFree)
	})

	t.Run("free to attach and back at detach", func(t *testing.T) {
		e := newEnv(t)
		a := e.attach(false)
		s := e.wantSlot(SlotAttach)
		if s.HolderID != a.HoldID || s.Label == "" || s.Since == "" {
			t.Errorf("attach slot = %+v, want hold %d, labelled and dated", s, a.HoldID)
		}
		e.releaseAttach(a.HoldID, "sess-1")
		e.wantSlot(SlotFree)
	})

	t.Run("a wake is held off while the hold lives", func(t *testing.T) {
		e := newEnv(t)
		e.acquire("bridge-a", "")
		if r := e.askForeman("while you hold"); r.Outcome != WakeHeld {
			t.Fatalf("ask under a hold = %s, want held", r.Outcome)
		}
		e.wantSlot(SlotInteractive)
	})
}

// beatUntil advances the clock in lease-sized steps with a heartbeat
// each, the way a live bridge does, until d has passed since the call.
func (e *env) beatUntil(h HoldResult, d time.Duration) {
	e.t.Helper()
	end := e.clock.Now().Add(d)
	for e.clock.Now().Before(end) {
		e.clock.Advance(30 * time.Second)
		if _, err := e.eng.HoldHeartbeat(e.ctx, h.HoldID, h.Fence); err != nil {
			return // the hold ended: the test reads why
		}
	}
	e.slot() // run a sweep at the final instant
}

// keepActiveUntil is beatUntil with a tool call every 5 minutes, so
// only the absolute bound can end the hold.
func (e *env) keepActiveUntil(h HoldResult, d time.Duration) {
	e.t.Helper()
	end := e.clock.Now().Add(d)
	last := e.clock.Now()
	for e.clock.Now().Before(end) {
		e.clock.Advance(30 * time.Second)
		if _, err := e.eng.HoldHeartbeat(e.ctx, h.HoldID, h.Fence); err != nil {
			return
		}
		if e.clock.Now().Sub(last) >= 5*time.Minute {
			if err := e.eng.HoldActivity(e.ctx, h.HoldID, h.Fence); err != nil {
				e.t.Fatalf("activity: %v", err)
			}
			last = e.clock.Now()
		}
	}
	e.slot()
}

func TestHoldAcquireStoresOnlyTheCredentialHash(t *testing.T) {
	e := newEnv(t)
	h := e.acquire("bridge-a", "")
	var stored string
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		return tx.QueryRow("SELECT credential_hash FROM interactive_holds WHERE id = ?", h.HoldID).Scan(&stored)
	}); err != nil {
		t.Fatal(err)
	}
	if stored == h.Credential || stored != hashToken(h.Credential) {
		t.Errorf("stored %q for credential %q, want its sha256 and never the credential", stored, h.Credential)
	}
	scope := func(secret string) (live, ok bool) {
		_, _, live, ok, err := e.eng.HoldCredentialScope(e.ctx, secret)
		if err != nil {
			t.Fatal(err)
		}
		return live, ok
	}
	if live, ok := scope(h.Credential); !live || !ok {
		t.Errorf("credential live/ok = %v/%v, want both", live, ok)
	}
	if err := e.eng.YieldHold(e.ctx, h.HoldID, h.Fence); err != nil {
		t.Fatal(err)
	}
	if live, ok := scope(h.Credential); live || !ok {
		t.Errorf("revoked credential live/ok = %v/%v, want known but not live", live, ok)
	}
}

func TestHoldAcquireDigest(t *testing.T) {
	e := newEnv(t)
	e.askForeman("before the hold") // wakes a turn; cancel it so the message returns to pending
	turn := e.wantSlot(SlotTurn)
	if _, err := e.eng.Cancel(e.ctx, turn.HolderID); err != nil {
		t.Fatal(err)
	}
	h := e.acquire("bridge-a", "")
	d := h.Digest
	if d == nil || len(d.Plans) != 1 || len(d.Plans[0].Tasks) != 1 {
		t.Fatalf("digest plans = %+v, want the fixture plan and its task", d)
	}
	if len(d.UserMessages) != 1 || d.UserMessages[0].Text != "before the hold" {
		t.Errorf("digest messages = %+v, want the waiting message", d.UserMessages)
	}
	// The digest reads; `events` consumes.
	in, err := e.eng.ForemanEvents(e.ctx, e.project)
	if err != nil || len(in.UserMessages) != 1 {
		t.Errorf("events after the digest = %+v, %v, want the message still pending", in, err)
	}
}

func TestHoldAcquireRefusals(t *testing.T) {
	t.Run("a headless turn names itself, its start and its events", func(t *testing.T) {
		e := newEnv(t)
		e.askForeman("please look")
		turn := e.wantSlot(SlotTurn)
		res := e.acquire("bridge-a", "")
		if res.Answer != AnsBusy || res.Busy == nil {
			t.Fatalf("acquire during a turn = %+v, want busy", res)
		}
		b := res.Busy
		if b.State != SlotTurn || b.HolderID != turn.HolderID || b.Since == "" || len(b.Events) != 1 {
			t.Errorf("holder = %+v, want the turn %d with its start and event", b, turn.HolderID)
		}
		if res.Credential != "" {
			t.Error("a refused acquire minted a credential")
		}
	})

	t.Run("an attach names itself, its label and since", func(t *testing.T) {
		e := newEnv(t)
		a := e.attach(false)
		res := e.acquire("bridge-a", "")
		if res.Answer != AnsBusy || res.Busy.State != SlotAttach || res.Busy.HolderID != a.HoldID ||
			res.Busy.Label == "" || res.Busy.Since == "" {
			t.Errorf("acquire during an attach = %+v, want busy naming the attach", res)
		}
	})

	t.Run("a second bridge instance is refused, naming the first", func(t *testing.T) {
		e := newEnv(t)
		first := e.acquire("bridge-a", "terminal one")
		second := e.acquire("bridge-b", "terminal two")
		if second.Answer != AnsBusy || second.Busy.State != SlotInteractive ||
			second.Busy.HolderID != first.HoldID || second.Busy.Label != "terminal one" || second.Busy.Since == "" {
			t.Errorf("second bridge = %+v, want busy naming hold %d and its label", second, first.HoldID)
		}
		// The first is untouched by the refusal.
		if err := e.eng.CheckHold(e.ctx, first.HoldID, first.Fence); err != nil {
			t.Errorf("first hold after the refusal: %v", err)
		}
	})

	t.Run("the same bridge instance replaces its own hold", func(t *testing.T) {
		e := newEnv(t)
		first := e.acquire("bridge-a", "")
		again := e.acquire("bridge-a", "")
		if again.Answer != AnsCreated || again.Fence <= first.Fence {
			t.Fatalf("reacquire = %+v, want created with a newer fence than %d", again, first.Fence)
		}
		staleReason(t, e.eng.CheckHold(e.ctx, first.HoldID, first.Fence))
		if err := e.eng.CheckHold(e.ctx, again.HoldID, again.Fence); err != nil {
			t.Errorf("the new hold: %v", err)
		}
	})

	t.Run("an attach is refused while the user's own session holds", func(t *testing.T) {
		e := newEnv(t)
		h := e.acquire("bridge-a", "")
		res := e.attach(true)
		if res.Answer != AnsBusy || res.InteractiveHoldID != h.HoldID {
			t.Errorf("attach under a hold = %+v, want busy naming interactive hold %d", res, h.HoldID)
		}
	})

	t.Run("an unknown project and a missing bridge instance", func(t *testing.T) {
		e := newEnv(t)
		if _, err := e.eng.AcquireHold(e.ctx, HoldAcquire{Project: 999, BridgeInstance: "b"}); !errors.Is(err, ErrNoProject) {
			t.Errorf("unknown project: err = %v, want ErrNoProject", err)
		}
		if _, err := e.eng.AcquireHold(e.ctx, HoldAcquire{Project: e.project}); err == nil {
			t.Error("acquire without a bridge instance succeeded")
		}
	})
}

// TestHoldFenceAndStale: the fence is monotonic per project, and a
// revoked credential or an old fence is rejected whole.
func TestHoldFenceAndStale(t *testing.T) {
	e := newEnv(t)
	first := e.acquire("bridge-a", "")
	if err := e.eng.CheckHold(e.ctx, first.HoldID, first.Fence); err != nil {
		t.Fatalf("a fresh hold with its fence: %v", err)
	}
	if s := staleReason(t, e.eng.CheckHold(e.ctx, first.HoldID, first.Fence-1)); !strings.Contains(s.Reason, "fence") {
		t.Errorf("old fence reason = %q, want it naming the fence", s.Reason)
	}
	if err := e.eng.YieldHold(e.ctx, first.HoldID, first.Fence); err != nil {
		t.Fatal(err)
	}
	if s := staleReason(t, e.eng.CheckHold(e.ctx, first.HoldID, first.Fence)); !strings.Contains(s.Reason, "yield") {
		t.Errorf("revoked reason = %q, want it naming the yield", s.Reason)
	}
	second := e.acquire("bridge-a", "")
	if second.Fence <= first.Fence+1 {
		t.Errorf("fences %d then %d: a hold end and an acquire each bump, so want a gap of at least 2", first.Fence, second.Fence)
	}
	// An old bridge's yield (a stale Stop hook) releases nothing.
	staleReason(t, e.eng.YieldHold(e.ctx, first.HoldID, first.Fence))
	e.wantSlot(SlotInteractive)
	if err := e.eng.CheckHold(e.ctx, second.HoldID, second.Fence); err != nil {
		t.Errorf("the newer hold after a stale yield: %v", err)
	}
}

// TestHoldStaleNamesATurn: when the slot went to a headless turn, the
// stale rejection says so.
func TestHoldStaleNamesATurn(t *testing.T) {
	e := newEnv(t)
	h := e.acquire("bridge-a", "")
	e.askForeman("wait for me") // held
	if err := e.eng.YieldHold(e.ctx, h.HoldID, h.Fence); err != nil {
		t.Fatal(err)
	}
	turn := e.wantSlot(SlotTurn) // the yield woke it
	s := staleReason(t, e.eng.CheckHold(e.ctx, h.HoldID, h.Fence))
	if s.Slot.State != SlotTurn || s.Slot.HolderID != turn.HolderID || len(s.Slot.Events) != 1 {
		t.Errorf("stale slot = %+v, want the turn %d and its event", s.Slot, turn.HolderID)
	}
}

// TestHoldExpiryClocks covers the two clocks with the fake clock:
// heartbeats prove liveness only, activity moves the idle bound only,
// and the absolute bound ends the hold whatever else happens.
func TestHoldExpiryClocks(t *testing.T) {
	t.Run("idle expiry after hold_idle_minutes without activity", func(t *testing.T) {
		e := newEnv(t)
		h := e.acquire("bridge-a", "")
		e.beatUntil(h, 14*time.Minute)
		e.wantSlot(SlotInteractive)
		e.beatUntil(h, 2*time.Minute) // heartbeats alone never extend past the idle bound
		e.wantSlot(SlotFree)
		if _, reason := e.holdRow(h.HoldID); reason != HoldEndIdle {
			t.Errorf("reason = %q, want idle", reason)
		}
		// A heartbeat never reacquires.
		staleReason(t, errOf(e.eng.HoldHeartbeat(e.ctx, h.HoldID, h.Fence)))
		e.wantSlot(SlotFree)
	})

	t.Run("activity moves the idle bound", func(t *testing.T) {
		e := newEnv(t)
		h := e.acquire("bridge-a", "")
		e.beatUntil(h, 14*time.Minute)
		if err := e.eng.HoldActivity(e.ctx, h.HoldID, h.Fence); err != nil {
			t.Fatal(err)
		}
		e.beatUntil(h, 14*time.Minute)
		e.wantSlot(SlotInteractive)
		e.beatUntil(h, 2*time.Minute)
		e.wantSlot(SlotFree)
	})

	t.Run("max expiry regardless of activity", func(t *testing.T) {
		e := newEnv(t)
		h := e.acquire("bridge-a", "")
		e.keepActiveUntil(h, 239*time.Minute)
		e.wantSlot(SlotInteractive)
		e.keepActiveUntil(h, 2*time.Minute)
		e.wantSlot(SlotFree)
		if _, reason := e.holdRow(h.HoldID); reason != HoldEndMax {
			t.Errorf("reason = %q, want max", reason)
		}
	})

	t.Run("a missed lease ends the hold at once", func(t *testing.T) {
		e := newEnv(t)
		h := e.acquire("bridge-a", "")
		e.clock.Advance(holdLeaseTTL - time.Second)
		if _, err := e.eng.HoldHeartbeat(e.ctx, h.HoldID, h.Fence); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(holdLeaseTTL - time.Second)
		e.wantSlot(SlotInteractive) // the beat renewed it
		e.clock.Advance(2 * time.Second)
		e.wantSlot(SlotFree)
		if _, reason := e.holdRow(h.HoldID); reason != HoldEndLease {
			t.Errorf("reason = %q, want lease", reason)
		}
	})

	t.Run("a heartbeat's lease never reaches past the activity bound", func(t *testing.T) {
		e := newEnv(t)
		h := e.acquire("bridge-a", "")
		e.beatUntil(h, 14*time.Minute+20*time.Second)
		lease, err := e.eng.HoldHeartbeat(e.ctx, h.HoldID, h.Fence)
		if err != nil {
			t.Fatal(err)
		}
		if bound := h.IdleExpires; lease.After(bound) {
			t.Errorf("lease until %s, past the idle bound %s", lease, bound)
		}
	})

	t.Run("the bounds are project settings", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingHoldIdleMinutes: "6", SettingHoldMaxMinutes: "10"})
		h := e.acquire("bridge-a", "")
		if got := h.IdleExpires.Sub(e.clock.Now()); got != 6*time.Minute {
			t.Errorf("idle bound = %s, want 6m", got)
		}
		e.keepActiveUntil(h, 9*time.Minute)
		e.wantSlot(SlotInteractive)
		e.keepActiveUntil(h, 2*time.Minute)
		if _, reason := e.holdRow(h.HoldID); reason != HoldEndMax {
			t.Errorf("reason = %q, want max at 10m", reason)
		}
	})
}

func errOf[T any](_ T, err error) error { return err }

func TestHoldSettingsValidated(t *testing.T) {
	e := newEnv(t)
	for _, v := range []string{"0", "-5", "soon", "1.5"} {
		for _, k := range []string{SettingHoldIdleMinutes, SettingHoldMaxMinutes} {
			if _, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{k: v}); !errors.Is(err, ErrInvalidSetting) {
				t.Errorf("%s=%s: err = %v, want ErrInvalidSetting", k, v, err)
			}
		}
	}
	idleOf := func(v ProjectSettings) SettingValue {
		for _, s := range v.Settings {
			if s.Key == SettingHoldIdleMinutes {
				return s
			}
		}
		t.Fatalf("no %s in %+v", SettingHoldIdleMinutes, v.Settings)
		return SettingValue{}
	}
	got := e.set(map[string]string{SettingHoldIdleMinutes: "30"})
	if v := idleOf(got); v.Key != SettingHoldIdleMinutes || v.Value != "30" || v.Default {
		t.Errorf("idle setting = %+v, want 30 overridden", v)
	}
	got = e.set(map[string]string{SettingHoldIdleMinutes: ""})
	if v := idleOf(got); v.Value != "15" || !v.Default {
		t.Errorf("reset idle setting = %+v, want the default 15", v)
	}
}

// TestHoldEndRevokesBumpsThenWakes pins the order of §4.6 "Before a
// wake": by the time the headless turn exists, the credential is dead and
// the fence is bumped, for every way a hold ends.
func TestHoldEndRevokesBumpsThenWakes(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(e *env, h HoldResult)
	}{
		{"yield", func(e *env, h HoldResult) {
			if err := e.eng.YieldHold(e.ctx, h.HoldID, h.Fence); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"forced release", func(e *env, _ HoldResult) {
			if _, err := e.eng.ReleaseHoldForced(e.ctx, e.project); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"idle expiry", func(e *env, h HoldResult) { e.beatUntil(h, 16*time.Minute) }},
		{"lost lease", func(e *env, _ HoldResult) { e.clock.Advance(time.Minute); e.slot() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			h := e.acquire("bridge-a", "")
			if r := e.askForeman("decide this"); r.Outcome != WakeHeld {
				t.Fatalf("ask under the hold = %s, want held", r.Outcome)
			}
			if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'foreman'"); n != 0 {
				t.Fatalf("%d foreman turns while the hold lived", n)
			}
			tc.end(e, h)

			turn := e.wantSlot(SlotTurn) // the end woke it, carrying the message
			if len(turn.Events) != 1 {
				t.Errorf("turn events = %v, want the held message", turn.Events)
			}
			if state, _ := e.holdRow(h.HoldID); state != "ended" {
				t.Errorf("hold state = %s, want ended: the credential is revoked", state)
			}
			if fence := e.int("SELECT hold_fence FROM projects WHERE id = ?", e.project); fence <= h.Fence {
				t.Errorf("project fence = %d, want it bumped past %d", fence, h.Fence)
			}
			// Order in the log: hold_ended carries the bumped fence and
			// lands before foreman_woken.
			var endedAt, wokenAt int64 = -1, -1
			var endedFence int64
			ids := e.eventIDsByKind()
			endedAt, wokenAt = ids[evHoldEnded], ids[evForemanWoken]
			if endedAt <= 0 || wokenAt <= 0 || endedAt >= wokenAt {
				t.Errorf("hold_ended event %d, foreman_woken event %d: want the end logged first", endedAt, wokenAt)
			}
			if p, ok := e.eventPayloadOf(evHoldEnded); !ok || p.Fence <= h.Fence {
				t.Errorf("hold_ended payload = %+v, want a fence past %d", p, h.Fence)
			} else {
				endedFence = p.Fence
			}
			if fence := e.int("SELECT hold_fence FROM projects WHERE id = ?", e.project); fence != endedFence {
				t.Errorf("project fence = %d, hold_ended says %d", fence, endedFence)
			}
			staleReason(t, e.eng.CheckHold(e.ctx, h.HoldID, h.Fence))
		})
	}
}

// eventIDsByKind maps each event kind to the id of its newest event.
func (e *env) eventIDsByKind() map[string]int64 {
	e.t.Helper()
	out := map[string]int64{}
	if err := e.st.View(e.ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT kind, MAX(id) FROM events GROUP BY kind")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var id int64
			if err := rows.Scan(&k, &id); err != nil {
				return err
			}
			out[k] = id
		}
		return rows.Err()
	}); err != nil {
		e.t.Fatalf("read event ids: %v", err)
	}
	return out
}

func TestForcedReleaseWithNoHoldIsGone(t *testing.T) {
	e := newEnv(t)
	if ans, err := e.eng.ReleaseHoldForced(e.ctx, e.project); err != nil || ans != AnsGone {
		t.Errorf("forced release of nothing = %v, %v, want gone", ans, err)
	}
	h := e.acquire("bridge-a", "")
	if ans, err := e.eng.ReleaseHoldForced(e.ctx, e.project); err != nil || ans != AnsOK {
		t.Fatalf("forced release = %v, %v, want ok", ans, err)
	}
	staleReason(t, errOf(e.eng.HoldHeartbeat(e.ctx, h.HoldID, h.Fence)))
	e.wantSlot(SlotFree)
}

// TestInFlightRevocationAppliesNothing: a call that passed its early
// fence check and then lost its hold — a forced release, an expiry — is
// re-validated inside the transaction that would apply it, and applies
// nothing.
func TestInFlightRevocationAppliesNothing(t *testing.T) {
	e := newEnv(t)
	h := e.acquire("bridge-a", "")
	ctx := WithHoldGuard(e.ctx, &HoldGuard{Hold: h.HoldID, Fence: h.Fence})
	if _, err := e.eng.ReleaseHoldForced(e.ctx, e.project); err != nil {
		t.Fatal(err)
	}
	plans := e.int("SELECT COUNT(*) FROM plans")
	jobs := e.int("SELECT COUNT(*) FROM jobs")
	_, err := e.eng.CreatePlan(ctx, e.project, "late", []PlanTask{{Title: "t", Spec: "s"}})
	staleReason(t, err)
	if _, err := e.eng.AmendPlan(ctx, e.project, e.plan, PlanAmendment{}); !errors.Is(err, ErrStaleHold) {
		t.Errorf("amend err = %v, want stale hold", err)
	}
	if _, err := e.eng.Cancel(ctx, 1); !errors.Is(err, ErrStaleHold) {
		t.Errorf("cancel err = %v, want stale hold", err)
	}
	if _, err := e.eng.Answer(ctx, e.project, e.task, "x"); !errors.Is(err, ErrStaleHold) {
		t.Errorf("answer err = %v, want stale hold", err)
	}
	if _, err := e.eng.RestartTask(ctx, e.project, e.task, "x"); !errors.Is(err, ErrStaleHold) {
		t.Errorf("restart err = %v, want stale hold", err)
	}
	if got := e.int("SELECT COUNT(*) FROM plans"); got != plans {
		t.Errorf("plans %d -> %d: a stale call applied", plans, got)
	}
	if got := e.int("SELECT COUNT(*) FROM jobs"); got != jobs {
		t.Errorf("jobs %d -> %d: a stale call applied", jobs, got)
	}
	// A stale expiry is caught too: the guard sweeps before it checks.
	h2 := e.acquire("bridge-a", "")
	ctx2 := WithHoldGuard(e.ctx, &HoldGuard{Hold: h2.HoldID, Fence: h2.Fence})
	e.clock.Advance(time.Hour)
	_, err = e.eng.CreatePlan(ctx2, e.project, "after expiry", []PlanTask{{Title: "t", Spec: "s"}})
	staleReason(t, err)
}

// TestOutstandingPollCannotConsumeForANewHold: hold A's long poll, still
// running after A was released and B acquired, consumes nothing of B's.
func TestOutstandingPollCannotConsumeForANewHold(t *testing.T) {
	e := newEnv(t)
	a := e.acquire("bridge-a", "")
	ctxA := WithHoldGuard(e.ctx, &HoldGuard{Hold: a.HoldID, Fence: a.Fence})
	if _, err := e.eng.ReleaseHoldForced(e.ctx, e.project); err != nil {
		t.Fatal(err)
	}
	b := e.acquire("bridge-b", "")
	e.eng.cfg.ForemanWakeBudget = 100
	if r := e.askForeman("for hold B"); r.Outcome != WakeHeld {
		t.Fatalf("ask = %s, want held", r.Outcome)
	}
	_, err := e.eng.ForemanEvents(ctxA, e.project)
	staleReason(t, err)
	ctxB := WithHoldGuard(e.ctx, &HoldGuard{Hold: b.HoldID, Fence: b.Fence})
	in, err := e.eng.ForemanEvents(ctxB, e.project)
	if err != nil || len(in.UserMessages) != 1 {
		t.Fatalf("B's poll = %+v, %v, want the message A's poll left alone", in, err)
	}
}

// TestForemanSubmissionRefusedWhileHeld: the common submission boundary
// refuses a headless turn while the user's session holds, whether it
// comes from the manual submit or the generic job submit.
func TestForemanSubmissionRefusedWhileHeld(t *testing.T) {
	e := newEnv(t)
	e.acquire("bridge-a", "")
	if _, err := e.eng.SubmitForemanTurn(e.ctx, e.project, []string{"x"}); !errors.Is(err, ErrSlotHeld) {
		t.Errorf("SubmitForemanTurn err = %v, want ErrSlotHeld", err)
	}
	_, err := e.eng.Submit(e.ctx, SubmitParams{Key: "foreman:1", ProjectID: e.project, Kind: KindForeman,
		Mode: ModeSession, Prompt: "wake"})
	if !errors.Is(err, ErrSlotHeld) {
		t.Errorf("Submit err = %v, want ErrSlotHeld", err)
	}
	if n := e.int("SELECT COUNT(*) FROM jobs WHERE kind = 'foreman'"); n != 0 {
		t.Errorf("%d foreman jobs exist under a hold", n)
	}
	e.wantSlot(SlotInteractive)
}
