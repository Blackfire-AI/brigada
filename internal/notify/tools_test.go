package notify

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// heldPipesScript is the hostile banner CLI: it stays alive until the
// context's kill lands on it, and a background child it spawns inherits
// the output pipes and holds them open — so the killed exec still has
// no EOF to wait for and an unbounded CombinedOutput blocks until the
// child exits. The script leaves its own pid and the holder's in
// markers, and the test's cleanup kills them: no orphan outlives it.
func heldPipesScript(markers string) string {
	return fmt.Sprintf(`
echo $$ > "%s/tool"
(sleep 600) &
echo $! > "%s/holder"
echo banner up
while :; do sleep 1; done
`, markers, markers)
}

// killScriptPids kills the scripted tool and its pipe holder.
func killScriptPids(t *testing.T, markers string) {
	t.Helper()
	var pids []int
	for _, name := range []string{"tool", "holder"} {
		if pid := testsupport.ReadPid(filepath.Join(markers, name)); pid > 0 {
			pids = append(pids, pid)
		}
	}
	testsupport.KillPids(t, pids...)
}

// TestOsascriptNotifyReturnsWhenTheToolHoldsItsPipes pins the bound
// (§10): the kill reaps the tool, but a child holding the pipes cannot
// hold Notify past the kill grace — it returns, with the exec's error,
// never with an unbounded wait.
func TestOsascriptNotifyReturnsWhenTheToolHoldsItsPipes(t *testing.T) {
	markers := t.TempDir()
	tool := testsupport.WriteScript(t, heldPipesScript(markers))
	t.Cleanup(func() { killScriptPids(t, markers) })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- Osascript{Tool: tool}.Notify(ctx, "title", "body") }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Notify = nil, want the error of a killed exec")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("Notify took %v on a killed exec, want it bounded soon after the kill grace", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Notify never returned: the holder child kept the pipes open past every bound")
	}
}

// TestNotifySendNotifyReturnsWhenTheToolHoldsItsPipes is the Linux
// notifier's share of the same bound.
func TestNotifySendNotifyReturnsWhenTheToolHoldsItsPipes(t *testing.T) {
	markers := t.TempDir()
	tool := testsupport.WriteScript(t, heldPipesScript(markers))
	t.Cleanup(func() { killScriptPids(t, markers) })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- NotifySend{Tool: tool}.Notify(ctx, "title", "body") }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Notify = nil, want the error of a killed exec")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("Notify took %v on a killed exec, want it bounded soon after the kill grace", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Notify never returned: the holder child kept the pipes open past every bound")
	}
}
