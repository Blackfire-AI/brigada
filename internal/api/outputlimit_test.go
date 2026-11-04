package api

import (
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// TestStatusSectionsShowsAnOutputLimitReason: a task parked by a run the
// output limit cut off reads its cut-off text in the needs-you list.
func TestStatusSectionsShowsAnOutputLimitReason(t *testing.T) {
	st := Status{NeedsYou: []engine.NeedsYouItem{{Kind: "task", ID: 4, State: "needs_foreman",
		Reason: "output limit reached: 32,000 tokens, 31,951 of them reasoning"}}}
	want := "task #4 needs_foreman: output limit reached: 32,000 tokens, 31,951 of them reasoning"
	if got := StatusSections(st); !strings.Contains(got, want) {
		t.Errorf("status sections = %q, want the line %q", got, want)
	}
}
