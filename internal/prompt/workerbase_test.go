package prompt

import "testing"

func TestWithWorkerBase(t *testing.T) {
	got := WithWorkerBase("the prompt\n", "brigada/landed", "abc123")
	want := "the prompt\n\nBase note: your worktree was built on \"brigada/landed\" at commit abc123. If your task is a wait_landed verification task, name in your result summary the head commit you verified.\n"
	if got != want {
		t.Errorf("WithWorkerBase = %q, want %q", got, want)
	}
}
