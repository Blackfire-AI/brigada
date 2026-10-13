package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/history"
)

// timeline renders a page's items as "type:kind" pairs, for order
// assertions.
func timeline(items []history.Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Type+":"+it.Kind)
	}
	return out
}

func TestHistoryMergesEventsAndMessagesOldestFirst(t *testing.T) {
	e := newEnv(t)

	// One run's whole story: the submit writes the created event and the
	// task's spec message, the lease and the completion add theirs, each
	// at its own instant.
	r := e.submit(e.work())
	e.clock.Advance(time.Minute)
	g := e.leaseAny(e.runner1)
	e.clock.Advance(time.Minute)
	e.complete(g, done())
	_ = r

	page, err := e.eng.History(e.ctx, HistoryQuery{})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	want := []string{
		"event:" + evJobCreated,
		"message:" + msgKindTaskSpec,
		"event:" + evJobLeased,
		"event:" + evJobCompleted,
		"message:" + msgKindWorkerResult,
	}
	if got := timeline(page.Items); !equalStrings(got, want) {
		t.Errorf("the timeline:\n got %v\nwant %v", got, want)
	}
	if page.NextCursor != "" {
		t.Errorf("the last page carries a cursor: %q", page.NextCursor)
	}

	// The items carry the references and the bodies.
	first := page.Items[0]
	if first.JobID != g.JobID || first.TaskID != e.task || first.ProjectID != e.project {
		t.Errorf("created event refs = project %d task %d job %d", first.ProjectID, first.TaskID, first.JobID)
	}
	spec := page.Items[1]
	if spec.Content != "the spec" || spec.Author != authorEngine || spec.EventID != first.ID {
		t.Errorf("task_spec message = %+v, want the spec, the engine's name, the created event", spec)
	}
	res := page.Items[4]
	if res.Author != authorWorker || res.EventID != page.Items[3].ID {
		t.Errorf("worker_result message = %+v, want the worker's name and the completed event", res)
	}
	if got := string(res.Content); got != `{"status":"done","summary":"finished"}` {
		t.Errorf("worker_result content = %s", got)
	}
}

func TestHistoryPaginatesByCursor(t *testing.T) {
	e := newEnv(t)

	// Five items at three distinct instants: two worker submits — on
	// two tasks, so the second creates rather than coalescing — and a
	// foreman turn between them.
	other := e.addTask("{}")
	e.submit(e.work())
	e.clock.Advance(time.Minute)
	e.submit(e.foremanTurn())
	e.clock.Advance(time.Minute)
	e.submit(SubmitParams{
		Key:       fmt.Sprintf("task:%d", other),
		ProjectID: e.project,
		TaskID:    other,
		Kind:      KindWork,
		Mode:      ModeFresh,
		Prompt:    "the other task's prompt",
	})

	var seen []string
	pages := 0
	var cursor string
	var lastItems []history.Item
	for {
		page, err := e.eng.History(e.ctx, HistoryQuery{Limit: 2, Since: cursor})
		if err != nil {
			t.Fatalf("history page %d: %v", pages+1, err)
		}
		seen = append(seen, timeline(page.Items)...)
		lastItems = page.Items
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("the timeline never ends")
		}
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
	want := []string{
		"event:" + evJobCreated, "message:" + msgKindTaskSpec,
		"event:" + evJobCreated,
		"event:" + evJobCreated, "message:" + msgKindTaskSpec,
	}
	if !equalStrings(seen, want) {
		t.Errorf("the pages, joined:\n got %v\nwant %v", seen, want)
	}

	// A cursor past the end answers an empty page, not an error.
	past := history.CursorOf(lastItems[len(lastItems)-1])
	page, err := e.eng.History(e.ctx, HistoryQuery{Limit: 2, Since: past})
	if err != nil || len(page.Items) != 0 || page.NextCursor != "" {
		t.Errorf("the page past the end = %d items, cursor %q, err %v", len(page.Items), page.NextCursor, err)
	}

	// A malformed cursor is the caller's mistake.
	if _, err := e.eng.History(e.ctx, HistoryQuery{Since: "not-a-cursor"}); err == nil {
		t.Error("a malformed cursor was accepted")
	}
}

func TestHistoryFiltersByProjectTaskAndJob(t *testing.T) {
	e := newEnv(t)

	// A second task and a second project, so each filter has something
	// to exclude.
	other := e.addTask("{}")
	e.exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES ('https://example.invalid/other.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, seedTS, seedTS)

	e.submit(e.work())
	r2 := e.submit(SubmitParams{
		Key:       fmt.Sprintf("task:%d", other),
		ProjectID: e.project,
		TaskID:    other,
		Kind:      KindWork,
		Mode:      ModeFresh,
		Prompt:    "the other task's prompt",
	})

	// The task filter narrows to one task's own story.
	page, err := e.eng.History(e.ctx, HistoryQuery{TaskID: other})
	if err != nil {
		t.Fatalf("history by task: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Kind != evJobCreated || page.Items[1].Kind != msgKindTaskSpec {
		t.Errorf("the task's timeline = %v", timeline(page.Items))
	}

	// The job filter is one job's whole story.
	page, err = e.eng.History(e.ctx, HistoryQuery{JobID: r2.JobID})
	if err != nil {
		t.Fatalf("history by job: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("the job's timeline = %v", timeline(page.Items))
	}

	// The project filter excludes what another project owns; here the
	// fixture's own.
	page, err = e.eng.History(e.ctx, HistoryQuery{ProjectID: e.project})
	if err != nil {
		t.Fatalf("history by project: %v", err)
	}
	if len(page.Items) != 4 {
		t.Errorf("the project's timeline = %v", timeline(page.Items))
	}
}
