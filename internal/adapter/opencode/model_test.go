package opencode

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestModelRejectedKind(t *testing.T) {
	for _, msg := range []string{
		"The model `example-model` does not exist or you do not have access to it.",
		"Model not found: example/example-model.",
	} {
		var e errorJSON
		e.Data.Message = msg
		e.Data.StatusCode = 400
		if got := errorKind(e); got != adapter.ErrModel {
			t.Errorf("errorKind(%q) = %q, want model", msg, got)
		}
	}
}
