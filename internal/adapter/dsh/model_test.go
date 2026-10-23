package dsh

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestModelRejectedKind(t *testing.T) {
	for _, e := range []errorJSON{
		{Status: 400, Message: "Model Not Exist"},
		{Status: 400, Message: "The model `example-model` does not exist"},
	} {
		if got := errorKind(e); got != adapter.ErrModel {
			t.Errorf("errorKind(%+v) = %q, want model", e, got)
		}
	}
}
