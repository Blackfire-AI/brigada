package kimi

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestModelRejectedClassification(t *testing.T) {
	a := &Adapter{}
	for _, line := range []string{
		"Error: failed to run prompt: Error code: 400 - invalid_request_error: the model `example-model` does not exist",
		"Error: failed to run prompt: Model not found: example-model",
	} {
		ev, ok := a.ParseErrEvent([]byte(line))
		if !ok || ev.ErrorKind != adapter.ErrModel {
			t.Errorf("ParseErrEvent(%q) = %+v, %v; want a model error", line, ev, ok)
		}
	}
}
