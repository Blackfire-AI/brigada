package history

import (
	"strings"
	"testing"
)

func TestMergeOrdersByTimeThenEventsThenMessages(t *testing.T) {
	events := []Event{
		{ID: 3, Kind: "job_completed", JobID: 1, CreatedAt: "2026-10-12T12:00:02.000000000Z"},
		{ID: 1, Kind: "job_created", JobID: 1, CreatedAt: "2026-10-12T12:00:00.000000000Z"},
		{ID: 2, Kind: "job_leased", JobID: 1, CreatedAt: "2026-10-12T12:00:01.000000000Z"},
	}
	messages := []Message{
		{ID: 2, Kind: "worker_result", Author: "worker", CreatedAt: "2026-10-12T12:00:02.000000000Z"},
		{ID: 1, Kind: "task_spec", Author: "engine", CreatedAt: "2026-10-12T12:00:00.000000000Z"},
	}
	items := Merge(events, messages)
	var order []string
	for _, it := range items {
		order = append(order, it.Type+":"+it.Kind)
	}
	want := []string{
		"event:job_created", "message:task_spec",
		"event:job_leased",
		"event:job_completed", "message:worker_result",
	}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Errorf("merged order = %v, want %v", order, want)
	}
}

func TestCursorRoundTrips(t *testing.T) {
	item := Item{Type: TypeEvent, ID: 42, CreatedAt: "2026-10-12T12:00:00.000000000Z"}
	cursor := CursorOf(item)
	key, ok := ParseCursor(cursor)
	if !ok {
		t.Fatalf("the cursor %q does not parse", cursor)
	}
	if key.CreatedAt != item.CreatedAt || key.Type != TypeEvent || key.ID != 42 {
		t.Errorf("parsed key = %+v, want the item's ordering fields", key)
	}
	for _, bad := range []string{"", "not-a-cursor", "!!!!", "YWJj"} { // "abc"
		if _, ok := ParseCursor(bad); ok {
			t.Errorf("cursor %q parsed, want it refused", bad)
		}
	}
}
