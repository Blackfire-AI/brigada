package history

import "testing"

// TestRenderOutputLimitFailure: a task_failed event parked by a run the
// output limit cut off shows its reason in the timeline.
func TestRenderOutputLimitFailure(t *testing.T) {
	it := Item{Type: TypeEvent, Kind: "task_failed", Payload: []byte(`{"status":"needs_foreman","reason":"output limit reached: 32,000 tokens, 31,951 of them reasoning"}`)}
	got := summary(it)
	want := "status=needs_foreman reason=output limit reached: 32,000 tokens, 31,951 of them reasoning"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
