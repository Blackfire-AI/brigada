package history

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// goldenItems is one small story: a task's spec, the run's events, the
// worker's answer — the shapes `brigada history` will render.
func goldenItems() []Item {
	return []Item{
		{
			Type: TypeEvent, ID: 1, Kind: "job_created", ProjectID: 7, TaskID: 3, JobID: 11,
			Payload:   []byte(`{"answer":"created","run":1}`),
			CreatedAt: "2026-10-12T12:00:00.000000000Z",
		},
		{
			Type: TypeMessage, ID: 1, Kind: "task_spec", ProjectID: 7, EventID: 1,
			Author:    "engine",
			Content:   "Add a README.\n\nIt should explain:\n" + "- what the tool does\n- how to run it",
			CreatedAt: "2026-10-12T12:00:00.000000000Z",
		},
		{
			Type: TypeEvent, ID: 2, Kind: "job_leased", ProjectID: 7, TaskID: 3, JobID: 11,
			Payload:   []byte(`{"lease":"2-abcd123","owner":2,"attempt":1}`),
			CreatedAt: "2026-10-12T12:00:01.000000000Z",
		},
		{
			Type: TypeEvent, ID: 3, Kind: "job_completed", ProjectID: 7, TaskID: 3, JobID: 11,
			Payload:   []byte(`{"status":"done","lease":"2-abcd123"}`),
			CreatedAt: "2026-10-12T12:04:00.000000000Z",
		},
		{
			Type: TypeMessage, ID: 2, Kind: "worker_result", ProjectID: 7, EventID: 3,
			Author:    "worker",
			Content:   `{"status":"done","summary":"added the README","branch":"task/3-1"}`,
			CreatedAt: "2026-10-12T12:04:00.000000000Z",
		},
	}
}

// TestRenderGolden is the plain-text rendering `brigada history` will
// sit on (§9): one line per item, bodies that run over several lines
// indented so items stay readable as blocks.
func TestRenderGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, goldenItems()); err != nil {
		t.Fatalf("render: %v", err)
	}
	golden := filepath.Join("testdata", "timeline.golden")
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read the golden file: %v", err)
	}
	if buf.String() != string(want) {
		t.Errorf("the rendering:\n%s\nwant the golden:\n%s", buf.String(), want)
	}
}
