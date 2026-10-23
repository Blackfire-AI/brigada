package runner

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/adapter/codex"
)

func feedCodex(r *jobRun, ad adapter.Adapter, lines ...string) {
	for _, l := range lines {
		r.recordEvent(ad, []byte(l), false)
	}
}

// TestCodexNoticeBeforeTurn: a codex error before turn.started is a
// warning on its job's own record, so a clean run keeps no CLI error and
// a failed one is named by turn.failed; an error after turn.started is
// the run's error. Jobs sharing one adapter keep their own turns.
func TestCodexNoticeBeforeTurn(t *testing.T) {
	const notice = `{"type":"error","message":"Reconnecting... 1/5"}`
	const started = `{"type":"turn.started"}`
	ad := &codex.Adapter{}
	newRun := func() *jobRun { return &jobRun{cfg: &Config{Clock: realClock()}} }

	clean := newRun()
	feedCodex(clean, ad, notice, started, `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
	if clean.cliError != nil {
		t.Errorf("clean run: cliError = %+v, want none", clean.cliError)
	}

	failed := newRun()
	feedCodex(failed, ad, notice, started, `{"type":"turn.failed","error":{"message":"the real failure"}}`)
	if failed.cliError == nil || failed.cliError.Text != "the real failure" || !failed.cliError.Final {
		t.Errorf("failed run: cliError = %+v, want the turn.failed one", failed.cliError)
	}

	// An earlier non-final error is replaced by a later turn.failed.
	late := newRun()
	feedCodex(late, ad, started, `{"type":"error","message":"first"}`, `{"type":"turn.failed","error":{"message":"closing"}}`)
	if late.cliError == nil || late.cliError.Text != "closing" {
		t.Errorf("late run: cliError = %+v, want the turn.failed text", late.cliError)
	}

	after := newRun()
	feedCodex(after, ad, started, `{"type":"item.completed","item":{"type":"error","message":"something odd"}}`)
	if after.cliError == nil || after.cliError.Text != "something odd" {
		t.Errorf("after turn.started: cliError = %+v, want the error", after.cliError)
	}

	// Another job's turn.started leaves this one's notice a warning.
	a, b := newRun(), newRun()
	feedCodex(b, ad, started)
	feedCodex(a, ad, notice)
	if a.cliError != nil {
		t.Errorf("shared adapter: cliError = %+v, want none", a.cliError)
	}
}
