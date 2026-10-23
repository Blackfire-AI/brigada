package claude

import (
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestModelRejectedClassification(t *testing.T) {
	a := &Adapter{}
	line := `{"type":"result","subtype":"success","is_error":true,"result":"API Error: 400 {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"There's an issue with the selected model (claude-example-1). It may not exist or you may not have access to it.\"}}"}`
	ev, ok := a.ParseEvent([]byte(line))
	if !ok || ev.Kind != adapter.EvError || ev.ErrorKind != adapter.ErrModel {
		t.Errorf("ParseEvent(%s) = %+v, %v; want a model error", line, ev, ok)
	}
	ev, ok = a.ParseErrEvent([]byte("There's an issue with the selected model (claude-example-1)."))
	if !ok || ev.ErrorKind != adapter.ErrModel {
		t.Errorf("stderr = %+v, %v; want a model error", ev, ok)
	}
}
