package history

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// Event is one row of the append-only event log (design §9) as the
// timeline shows it: the kind, the references, the time and the small
// JSON payload the engine recorded with the change.
type Event struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	ProjectID int64           `json:"project_id"`
	TaskID    int64           `json:"task_id,omitempty"`
	JobID     int64           `json:"job_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt string          `json:"created_at"`
}

// Message is one conversation item of a project's history (§9): a task
// spec, a worker result, and the items the foreman's later turns add.
// The kinds stay an open set; each message names the event that produced
// it, when there is one.
type Message struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	EventID   int64  `json:"event_id,omitempty"`
	Kind      string `json:"kind"`
	Author    string `json:"author"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// The item types of the merged timeline. They also serve as the rank
// within one instant: "event" sorts before "message" ("e" < "m" in the
// store's plain text order), so two rows written in one transaction —
// the change and the message it produced — appear in the order they
// were written.
const (
	TypeEvent   = "event"
	TypeMessage = "message"
)

// Item is one line of the merged timeline: an event or a message, with
// the fields that belong to its type carrying data and the rest empty.
type Item struct {
	Type      string          `json:"type"`
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	ProjectID int64           `json:"project_id"`
	TaskID    int64           `json:"task_id,omitempty"`
	JobID     int64           `json:"job_id,omitempty"`
	EventID   int64           `json:"event_id,omitempty"` // a message's producing event
	Author    string          `json:"author,omitempty"`   // a message's author
	Content   string          `json:"content,omitempty"`  // a message's body
	Payload   json.RawMessage `json:"payload,omitempty"`  // an event's data
	CreatedAt string          `json:"created_at"`
}

// Page is one window of the timeline: the items, oldest first, and the
// cursor the next page continues from — empty on the last page.
type Page struct {
	Items      []Item `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Merge folds events and messages into one timeline, oldest first: by
// time, then events before messages within one instant — the change a
// transaction made precedes the message it produced there — then by
// id. Timestamps are the store's fixed-width UTC text (§11), so plain
// text order is chronological order.
func Merge(events []Event, messages []Message) []Item {
	items := make([]Item, 0, len(events)+len(messages))
	for _, ev := range events {
		items = append(items, Item{
			Type: TypeEvent, ID: ev.ID, Kind: ev.Kind,
			ProjectID: ev.ProjectID, TaskID: ev.TaskID, JobID: ev.JobID,
			Payload: ev.Payload, CreatedAt: ev.CreatedAt,
		})
	}
	for _, m := range messages {
		items = append(items, Item{
			Type: TypeMessage, ID: m.ID, Kind: m.Kind,
			ProjectID: m.ProjectID, EventID: m.EventID,
			Author: m.Author, Content: m.Content, CreatedAt: m.CreatedAt,
		})
	}
	slices.SortStableFunc(items, compare)
	return items
}

// compare is the timeline's order: time, then type, then id.
func compare(a, b Item) int {
	if c := strings.Compare(a.CreatedAt, b.CreatedAt); c != 0 {
		return c
	}
	if c := strings.Compare(a.Type, b.Type); c != 0 {
		return c
	}
	return cmpInt64(a.ID, b.ID)
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// CursorOf renders the cursor of item: where a page ended, so the next
// one continues after exactly it. Opaque by design — the format may
// change — and safe in a query parameter.
func CursorOf(item Item) string {
	raw := strings.Join([]string{item.CreatedAt, item.Type, strconv.FormatInt(item.ID, 10)}, "\x1f")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// CursorKey is a parsed cursor: the item a page ended on, with only the
// three ordering fields set.
type CursorKey struct {
	CreatedAt string
	Type      string
	ID        int64
}

// ParseCursor reads a cursor CursorOf produced; ok is false for a
// malformed one, which the caller treats as "start from the beginning"
// or refuses, as it prefers.
func ParseCursor(s string) (CursorKey, bool) {
	if s == "" {
		return CursorKey{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return CursorKey{}, false
	}
	at, rest, ok := strings.Cut(string(raw), "\x1f")
	if !ok {
		return CursorKey{}, false
	}
	typ, id, ok := strings.Cut(rest, "\x1f")
	if !ok || (typ != TypeEvent && typ != TypeMessage) {
		return CursorKey{}, false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || at == "" {
		return CursorKey{}, false
	}
	return CursorKey{CreatedAt: at, Type: typ, ID: n}, true
}
