// The hostile banner CLI driven through the engine role itself: the
// scripted tool's exec never ends on its own — the kill reaps the tool,
// but a background child holds the output pipes open — so the delivery
// sticks and the engine's tick and shutdown must both work around it.
// (The file name is a leftover: this session's shell rules allow no
// rename.)
package lifecycle_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// bannerToolScript is the hostile banner CLI: it stays alive until the
// context's kill lands on it, and a background child it spawns inherits
// the output pipes and holds them open — so the killed exec still has
// no EOF and the delivery stays in flight for far longer than a tick.
// The script leaves its own pid and the holder's in markers for the
// test's cleanup, so no orphan process outlives it.
func bannerToolScript(markers string) string {
	return fmt.Sprintf(`
echo $$ > "%s/tool"
(sleep 600) &
echo $! > "%s/holder"
echo banner up
while :; do sleep 1; done
`, markers, markers)
}

// fakeBannerTools puts the hostile banner CLI on PATH under both
// notifier names — osascript and notify-send — so whichever platform
// the test runs on, the engine's auto-detected notifier is the fake.
func fakeBannerTools(t *testing.T, markers string) []string {
	t.Helper()
	body := bannerToolScript(markers)
	return []string{
		filepath.Dir(testsupport.WriteNamedScript(t, "osascript", body)),
		filepath.Dir(testsupport.WriteNamedScript(t, "notify-send", body)),
	}
}

// killBannerTools kills the scripted banner tool and its pipe holder.
func killBannerTools(t *testing.T, markers string) {
	t.Helper()
	var pids []int
	for _, name := range []string{"tool", "holder"} {
		if pid := testsupport.ReadPid(filepath.Join(markers, name)); pid > 0 {
			pids = append(pids, pid)
		}
	}
	testsupport.KillPids(t, pids...)
}

// TestRunStuckDeliveryDoesNotStopTheTick wires the real notifier to the
// hostile banner tool over a store holding one parking event: the first
// tick's delivery sticks, and the tick must go on around it — a
// snapshot deleted mid-run is taken again by a later tick (past the
// sweep, in the same tick body), and the idle exit is reached and
// served while the banner exec still runs.
func TestRunStuckDeliveryDoesNotStopTheTick(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	p := lifecycle.PathsOf(state)
	snaps := filepath.Join(state, "snapshots")

	markers := t.TempDir()
	tools := fakeBannerTools(t, markers)
	t.Setenv("PATH", strings.Join(tools, string(os.PathListSeparator))+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { killBannerTools(t, markers) })

	// One parking event, so the first tick's flush has a banner to show.
	ctx := context.Background()
	st, err := store.Open(ctx, p.Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	seedNotifyEvent(t, ctx, st)
	st.Close()

	block := make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		runDone <- lifecycle.Run(ctx, lifecycle.Config{
			StateDir:    state,
			SnapshotDir: snaps,
			Idle:        2500 * time.Millisecond,
			IdleTick:    50 * time.Millisecond,
			FlushGrace:  500 * time.Millisecond,
			FlushNotifications: func(context.Context) error {
				<-block
				return nil
			},
		})
	}()

	// The startup snapshot (taken before the API serves, with the first
	// tick's delivery about to stick) is the one the test deletes.
	snapshotFiles := func() []string {
		entries, err := os.ReadDir(snaps)
		if err != nil {
			return nil
		}
		var out []string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "store-") && strings.HasSuffix(e.Name(), ".db") {
				out = append(out, e.Name())
			}
		}
		return out
	}
	waitFor(t, "the startup snapshot", func() bool { return len(snapshotFiles()) == 1 })
	first := snapshotFiles()[0]

	// Past the stuck first tick now. Delete the snapshot and wait for a
	// later tick to take another: the tick body — sweep, snapshot, idle
	// check — must keep running while the delivery is in flight. The
	// pause lands the new file in another second, so its name differs.
	time.Sleep(1100 * time.Millisecond)
	if err := os.Remove(filepath.Join(snaps, first)); err != nil {
		t.Fatalf("remove the startup snapshot: %v", err)
	}
	waitFor(t, "a later tick to snapshot again", func() bool { return len(snapshotFiles()) == 1 })

	// The idle exit is served too, with the delivery still in flight:
	// the shutdown stops it within the grace and moves on.
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run the engine: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("the idle exit never came: a stuck banner exec holds the tick, so the engine cannot idle out")
	}
	close(block)
}

// TestRunCancelWhileStuckReturnsWithinTheGrace cancels the engine's
// context while a banner delivery is in flight: the shutdown must
// return within the one grace plus slack — the in-flight delivery is
// abandoned at it, never waited out (and never given a second wait
// beside it).
func TestRunCancelWhileStuckReturnsWithinTheGrace(t *testing.T) {
	state := testsupport.ShortStateDir(t)
	p := lifecycle.PathsOf(state)

	markers := t.TempDir()
	tools := fakeBannerTools(t, markers)
	t.Setenv("PATH", strings.Join(tools, string(os.PathListSeparator))+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { killBannerTools(t, markers) })

	ctx := context.Background()
	st, err := store.Open(ctx, p.Store)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	seedNotifyEvent(t, ctx, st)
	st.Close()

	const grace = time.Second
	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() {
		runDone <- lifecycle.Run(runCtx, lifecycle.Config{
			StateDir:   state,
			Idle:       time.Hour,
			IdleTick:   50 * time.Millisecond,
			FlushGrace: grace,
		})
	}()
	time.Sleep(600 * time.Millisecond) // the delivery is in flight by now
	begin := time.Now()
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run the engine: %v", err)
		}
		if elapsed := time.Since(begin); elapsed > 1700*time.Millisecond {
			t.Errorf("the ctx-cancel shutdown took %v with a delivery in flight, want at most the grace plus slack", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ctx-cancel shutdown never returned: a stuck banner exec holds the main loop")
	}
}
