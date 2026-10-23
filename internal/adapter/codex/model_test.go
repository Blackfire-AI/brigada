package codex

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestModelRejectedClassification(t *testing.T) {
	a := &Adapter{}
	for _, line := range []string{
		`{"type":"turn.failed","error":{"message":"unexpected status 400 Bad Request: The 'example-model' model is not supported when using Codex with a ChatGPT account."}}`,
		`{"type":"error","message":"The 'example-model' model is not supported when using Codex with a ChatGPT account."}`,
	} {
		ev, ok := a.ParseEvent([]byte(line))
		if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != adapter.ErrModel {
			t.Errorf("ParseEvent(%s) = %+v, %v; want a model error", line, ev, ok)
		}
	}
	// The startup notice about missing metadata is no refusal.
	ev, _ := a.ParseEvent([]byte(`{"type":"error","message":"Model metadata for example-model not found. Defaulting to fallback metadata."}`))
	if ev.ErrorKind == adapter.ErrModel {
		t.Errorf("metadata notice classed as %q", ev.ErrorKind)
	}
}
