package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/foreman"
)

// eventsArgs is the events tool's arguments (§4.2, §12.2): the
// client-given long-poll wait in seconds.
type eventsArgs struct {
	WaitSeconds float64 `json:"wait_seconds"`
}

// callEvents runs the events tool (§4.2, §12.2): the project's pending
// judgment events through /v1/events, rendered exactly as a wake prompt
// carries them — the same data delimiters, the same verbatim lines — so
// what the attached foreman reads is what a headless turn would have
// woken with. Reading them consumes them (§4.2); an empty answer says
// there is nothing new.
func callEvents(ctx context.Context, c *api.Client, args json.RawMessage) (string, error) {
	var a eventsArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	if a.WaitSeconds < 0 {
		a.WaitSeconds = 0
	}
	inbox, err := c.ForemanEvents(ctx, time.Duration(a.WaitSeconds*float64(time.Second)))
	if err != nil {
		return "", err
	}
	if inbox.Empty() {
		return "no new events", nil
	}
	// The events come first, as data; the user's messages follow in their
	// one framed section (§4.2 item 3), exactly as a wake prompt carries
	// both — the framing travels with the section, in this same answer.
	return foreman.EventsAnswer(inbox.Events, inbox.UserMessages), nil
}
