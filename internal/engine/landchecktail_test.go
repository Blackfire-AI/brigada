package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTailNote: the helper keeps the end of a log within the bound, on a
// rune boundary.
func TestTailNote(t *testing.T) {
	if got := tailNote("short", 500); got != "short" {
		t.Errorf("tailNote of a short log = %q, want it whole", got)
	}
	if got := tailNote("abc", 0); got != "" {
		t.Errorf("tailNote with no room = %q, want empty", got)
	}
	// 3-byte runes: a cut at 4 bytes lands inside the second rune from the end.
	s := strings.Repeat("世", 10)
	got := tailNote(s, 4)
	if got != "世" || !utf8.ValidString(got) {
		t.Errorf("tailNote(%q, 4) = %q, want the last whole rune", s, got)
	}
	if got := tailNote("start-middle-END", 3); got != "END" {
		t.Errorf("tailNote = %q, want the last bytes", got)
	}
}

// TestLandCheckFailedKeepsLogEnd: a long check log whose start and end
// differ reaches the event as its end, in the reason within its bound and
// in the check output, and the reason names the land job.
func TestLandCheckFailedKeepsLogEnd(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement")
	reviewer := e.agented("reviewer", "google", "implement", "review")
	e.runToDone(author, reviewer)

	g := e.leaseGit(author)
	if g == nil {
		t.Fatal("the land job was not leased")
	}
	log := "HEADLINE-START\n" + strings.Repeat("世界 noise line\n", 200) + "FAIL TestTheEnd"
	res, err := json.Marshal(map[string]any{"target_branch": "main", "commit": "", "check_passed": false, "check_log": log})
	if err != nil {
		t.Fatal(err)
	}
	if ans := e.complete(g, Completion{Result: string(res)}); ans != AnsOK {
		t.Fatalf("complete the land job: %s", ans)
	}
	p, ok := e.eventPayloadOf(evTaskLandFailed)
	if !ok {
		t.Fatal("no task_land_failed event")
	}
	if len(p.Reason) > noteBound || len(p.CheckOutput) > noteBound {
		t.Errorf("reason %d bytes, check output %d bytes, want each within %d", len(p.Reason), len(p.CheckOutput), noteBound)
	}
	for name, got := range map[string]string{"reason": p.Reason, "check output": p.CheckOutput} {
		if !strings.HasSuffix(got, "FAIL TestTheEnd") || strings.Contains(got, "HEADLINE-START") || !utf8.ValidString(got) {
			t.Errorf("%s = %q, want the end of the log on rune boundaries", name, got)
		}
	}
	if want := fmt.Sprintf("(land job %d)", g.JobID); !strings.Contains(p.Reason, want) {
		t.Errorf("reason = %q, want it to name %s", p.Reason, want)
	}
	if landRetryablePush(p) {
		t.Error("a failed land check reads as a retryable push")
	}
}
