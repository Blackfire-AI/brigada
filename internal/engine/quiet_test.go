package engine

import (
	"fmt"
	"testing"
	"time"
)

// The interactive quiet period (§4.6, decided 2026-10-20): while the
// project's interactive hold is held, or was released less than
// interactive_quiet_minutes ago, an event does not wake the headless
// foreman. The events wait, the next hold_acquire digest carries them,
// and when the quiet lapses without a new acquire the sweep wakes one
// turn for everything that waited.

// quietHold runs one acquire-yield cycle with nothing pending, so the
// quiet window begins at a known moment and no deferral is recorded by
// the yield itself.
func quietHold(e *env) {
	e.t.Helper()
	h := e.acquire("bridge-a", "my session")
	if err := e.eng.YieldHold(e.ctx, h.HoldID, h.Fence); err != nil {
		e.t.Fatal(err)
	}
}

// wantNoTurn asserts no headless turn exists.
func (e *env) wantNoTurn(why string) {
	e.t.Helper()
	if got := e.foremanJobs(); got != 0 {
		e.t.Fatalf("%s: foreman turns = %d, want 0", why, got)
	}
}

func TestInteractiveQuietDefersWakes(t *testing.T) {
	t.Run("an event during the hold waits, as before", func(t *testing.T) {
		e := newEnv(t)
		e.acquire("bridge-a", "")
		e.park("which file?")
		e.wantNoTurn("the hold keeps the slot")
		if got := e.countEvents(evForemanWakeDeferred); got != 0 {
			t.Errorf("deferral events under a live hold = %d, want 0", got)
		}
		if want := []string{"pending:0"}; !equalStrings(e.inboxStates(), want) {
			t.Errorf("inbox = %v, want the event pending", e.inboxStates())
		}
	})

	t.Run("an event inside the quiet minutes waits and is recorded once", func(t *testing.T) {
		e := newEnv(t)
		quietHold(e)
		e.park("which file?")
		e.wantNoTurn("the quiet period holds the wake")
		if want := []string{"pending:0"}; !equalStrings(e.inboxStates(), want) {
			t.Errorf("inbox = %v, want the event pending", e.inboxStates())
		}
		if got := e.countEvents(evForemanWakeDeferred); got != 1 {
			t.Fatalf("deferral events = %d, want 1", got)
		}
		// A second event in the same quiet window defers without a
		// second record, like the budget's one event per window.
		if r := e.askForeman("and this too"); r.Outcome != WakeQuiet || r.QuietUntil == "" {
			t.Fatalf("ask inside the quiet = %s until %q, want quiet with an end", r.Outcome, r.QuietUntil)
		}
		if got := e.countEvents(evForemanWakeDeferred); got != 1 {
			t.Errorf("deferral events after the second event = %d, want still 1", got)
		}
		e.wantNoTurn("the quiet period still holds the wake")
	})

	t.Run("a wake fires once when the quiet lapses, without a new event", func(t *testing.T) {
		e := newEnv(t)
		quietHold(e)
		e.park("which file?")
		e.askForeman("and this too")
		e.wantNoTurn("the quiet period holds the wake")
		e.clock.Advance(defaultQuietMinutes*time.Minute + time.Second)
		e.mustSweep() // nothing but time passed
		if got := e.foremanJobs(); got != 1 {
			t.Fatalf("foreman turns after the lapse = %d, want 1", got)
		}
		job := e.int("SELECT id FROM jobs WHERE kind = 'foreman'")
		want := []string{"delivered:" + fmt.Sprint(job), "delivered:" + fmt.Sprint(job)}
		if !equalStrings(e.inboxStates(), want) {
			t.Errorf("inbox after the lapse = %v, want both rows on turn %d", e.inboxStates(), job)
		}
		// One turn carried everything; another sweep adds nothing.
		e.mustSweep()
		if got := e.foremanJobs(); got != 1 {
			t.Errorf("foreman turns after a second sweep = %d, want still 1", got)
		}
	})

	t.Run("a new hold_acquire inside the quiet carries the digest", func(t *testing.T) {
		e := newEnv(t)
		quietHold(e)
		e.park("which file?")
		h := e.acquire("bridge-a", "back again")
		if len(h.Digest.Events) != 1 {
			t.Fatalf("digest events = %v, want the waiting event", h.Digest.Events)
		}
		e.wantNoTurn("the reacquired hold keeps the slot")
	})

	t.Run("zero disables the quiet period", func(t *testing.T) {
		e := newEnv(t)
		e.set(map[string]string{SettingInteractiveQuietMinutes: "0"})
		quietHold(e)
		e.park("which file?")
		if got := e.foremanJobs(); got != 1 {
			t.Fatalf("foreman turns with the quiet off = %d, want 1", got)
		}
		if got := e.countEvents(evForemanWakeDeferred); got != 0 {
			t.Errorf("deferral events with the quiet off = %d, want 0", got)
		}
	})

	t.Run("the setting is validated and shown", func(t *testing.T) {
		e := newEnv(t)
		for _, v := range []string{"-1", "soon", "1.5"} {
			if _, err := e.eng.SetProjectSettings(e.ctx, envRepo, map[string]string{SettingInteractiveQuietMinutes: v}); err == nil {
				t.Errorf("%s accepted", v)
			}
		}
		got := e.set(map[string]string{SettingInteractiveQuietMinutes: "0"})
		quietOf := func(v ProjectSettings) SettingValue {
			for _, s := range v.Settings {
				if s.Key == SettingInteractiveQuietMinutes {
					return s
				}
			}
			t.Fatalf("no %s in %+v", SettingInteractiveQuietMinutes, v.Settings)
			return SettingValue{}
		}
		if v := quietOf(got); v.Value != "0" || v.Default {
			t.Errorf("quiet setting = %+v, want 0 overridden", v)
		}
		got = e.set(map[string]string{SettingInteractiveQuietMinutes: ""})
		if v := quietOf(got); v.Value != "10" || !v.Default {
			t.Errorf("reset quiet setting = %+v, want the default 10", v)
		}
	})

	t.Run("the needs-you queue is unaffected", func(t *testing.T) {
		e := newEnv(t)
		quietHold(e)
		e.park("which file?") // the notification's own event (§10)
		st, err := e.eng.Status(e.ctx, e.project)
		if err != nil {
			t.Fatal(err)
		}
		if e.countEvents(evTaskNeedsHelp) != 1 {
			t.Errorf("task_needs_help events = %d, want 1: the notification reads it (§10)", e.countEvents(evTaskNeedsHelp))
		}
		found := false
		for _, n := range st.NeedsYou {
			if n.Kind == "task" && n.ID == e.task && n.State == "needs_foreman" {
				found = true
			}
		}
		if !found {
			t.Errorf("needs you = %+v, want task %d parked", st.NeedsYou, e.task)
		}
		e.wantNoTurn("the quiet period holds the wake")
	})

	t.Run("status names the deferral and its end", func(t *testing.T) {
		e := newEnv(t)
		h := e.acquire("bridge-a", "")
		e.park("which file?")
		st, err := e.eng.Status(e.ctx, e.project)
		if err != nil {
			t.Fatal(err)
		}
		// Under a live hold the wake is deferred to the hold's earliest
		// end plus the quiet minutes.
		if len(st.DeferredWakes) != 1 || st.DeferredWakes[0].ProjectID != e.project {
			t.Fatalf("deferred wakes under the hold = %+v, want one of this project", st.DeferredWakes)
		}
		want := h.IdleExpires.Add(defaultQuietMinutes * time.Minute)
		if got := st.DeferredWakes[0].Until; got != fmtTime(want) {
			t.Errorf("deferred until = %s, want the idle bound %s plus the quiet", got, fmtTime(want))
		}
		if err := e.eng.YieldHold(e.ctx, h.HoldID, h.Fence); err != nil {
			t.Fatal(err)
		}
		st, err = e.eng.Status(e.ctx, e.project)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.DeferredWakes) != 1 {
			t.Fatalf("deferred wakes inside the quiet = %+v, want one", st.DeferredWakes)
		}
		e.quietLapse()
		st, err = e.eng.Status(e.ctx, e.project)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.DeferredWakes) != 0 {
			t.Errorf("deferred wakes after the lapse = %+v, want none", st.DeferredWakes)
		}
	})
}
