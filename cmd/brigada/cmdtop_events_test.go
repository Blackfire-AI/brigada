package main

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/history"
	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// evAt is an event of kind for task at hour:min on a day of October 2026
// (topNow is on the 20th).
func evAt(id int64, kind string, task int64, day, hour, min int, payload string) history.Item {
	at := time.Date(2026, 10, day, hour, min, 0, 0, time.UTC)
	return history.Item{Type: history.TypeEvent, ID: id, Kind: kind, ProjectID: 1, TaskID: task,
		Payload: []byte(payload), CreatedAt: at.Format(engine.TimeLayout)}
}

// tableStatus is a status whose plan 107 and tasks 355 and 353 have the
// titles the EVENTS table shows.
func tableStatus() api.Status {
	return api.Status{Plans: []engine.StatusPlan{{ID: 107, Title: "top keeps its last column and redraws on resize",
		Tasks: []engine.StatusTask{
			{ID: 355, Title: "CHANGELOG entry for #181"},
			{ID: 353, Title: "top: keep the last column and redraw on resize"},
		}}}}
}

// tableItems are the table's events, oldest first; the review falls on
// reviewDay.
func tableItems(reviewDay int) []history.Item {
	until := topNow.Add(time.Hour).UTC().Format(engine.TimeLayout)
	return []history.Item{
		evAt(1, "review_verdict", 0, reviewDay, 1, 57, `{"status":"approve","branch":"brigada/landed"}`),
		evAt(2, "agent_cooling", 353, 20, 14, 36, `{"account":"glm-5.3","until":"`+until+`"}`),
		evAt(3, "task_landed", 353, 20, 14, 55, `{}`),
		evAt(4, "task_landed", 355, 20, 15, 6, `{}`),
		evAt(5, "plan_done", 353, 20, 15, 6, `{"plan":107}`),
	}
}

// TestTopEventsTable: the rows read as a table; a plan_done with a plan and
// a task id shows the plan's number and title, an agent_cooling that
// carries a task id shows no number, and a date row widens the time column
// of every row.
func TestTopEventsTable(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	want := []string{
		"EVENTS",
		"15:06  plan    #107  done     top keeps its last column and redraws on resize",
		"15:06  task    #355  landed   CHANGELOG entry for #181",
		"14:55  task    #353  landed   top: keep the last column and redraw on resize",
		"14:36  agent         cooling  glm-5.3 until 16:04",
		"01:57  review        approve  brigada/landed",
	}
	if got := topEventTexts(tableItems(20), tableStatus(), topNow, style, 100); !reflect.DeepEqual(got, want) {
		t.Errorf("table =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	wantDate := []string{
		"EVENTS",
		"15:06        plan    #107  done     top keeps its last column and redraws on resize",
		"15:06        task    #355  landed   CHANGELOG entry for #181",
		"14:55        task    #353  landed   top: keep the last column and redraw on resize",
		"14:36        agent         cooling  glm-5.3 until 16:04",
		"10-19 01:57  review        approve  brigada/landed",
	}
	if got := topEventTexts(tableItems(19), tableStatus(), topNow, style, 100); !reflect.DeepEqual(got, wantDate) {
		t.Errorf("table with a date row =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantDate, "\n"))
	}
}

// TestTopEventsReviewTitle: a review with a task number shows the task's
// title, one with none its branch, and one whose task is gone a blank title
// and not the branch; a plan or task gone from the status is blank too.
func TestTopEventsReviewTitle(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	items := []history.Item{
		evAt(1, "review_verdict", 3, 20, 15, 0, `{"status":"changes","branch":"fix/x"}`),
		evAt(2, "review_verdict", 0, 20, 15, 1, `{"status":"approve","branch":"fix/y"}`),
		evAt(3, "review_verdict", 9, 20, 15, 2, `{"status":"approve","branch":"fix/z"}`),
		evAt(4, "task_failed", 8, 20, 15, 3, `{}`),
		evAt(5, "plan_approved", 0, 20, 15, 4, `{"plan":5}`),
	}
	want := []string{
		"EVENTS",
		"15:04  plan    #5  approved",
		"15:03  task    #8  failed",
		"15:02  review  #9  approve",
		"15:01  review      approve   fix/y",
		"15:00  review  #3  changes   oil the gears",
	}
	got := topEventTexts(items, topTitleStatus("p", "oil the gears"), topNow, style, 100)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTopEventsHideLandedDone: a landed task's done row is hidden whichever
// event is older and before the 10-row limit; a done task without a landed
// event, a class that never lands, and failed, land-failed and pushed rows
// stay.
func TestTopEventsHideLandedDone(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	texts := func(items ...history.Item) string {
		return strings.Join(topEventTexts(items, api.Status{}, topNow, style, 100)[1:], "\n")
	}
	if got := texts(evAt(1, "task_landed", 3, 20, 15, 0, `{}`), evAt(2, "task_done", 3, 20, 15, 1, `{}`)); got != "15:00  task  #3  landed" {
		t.Errorf("landed older than done: %q", got)
	}
	if got := texts(evAt(1, "task_done", 3, 20, 15, 0, `{}`), evAt(2, "task_landed", 3, 20, 15, 1, `{}`)); got != "15:01  task  #3  landed" {
		t.Errorf("done older than landed: %q", got)
	}
	if got := texts(evAt(1, "task_done", 5, 20, 15, 0, `{}`), evAt(2, "task_landed", 6, 20, 15, 1, `{}`)); got != "15:01  task  #6  landed\n15:00  task  #5  done" {
		t.Errorf("an unlanded done lost its row: %q", got)
	}
	if got := texts(evAt(1, "task_done", 5, 20, 15, 0, `{}`)); got != "15:00  task  #5  done" {
		t.Errorf("a task of a class that never lands lost its row: %q", got)
	}
	got := texts(evAt(1, "task_done", 4, 20, 15, 0, `{}`), evAt(2, "task_failed", 4, 20, 15, 1, `{}`),
		evAt(3, "task_land_failed", 4, 20, 15, 2, `{}`), evAt(4, "task_pushed", 4, 20, 15, 3, `{}`), evAt(5, "task_landed", 4, 20, 15, 4, `{}`))
	if want := "15:04  task  #4  landed      \n15:03  task  #4  pushed      \n15:02  task  #4  land failed\n15:01  task  #4  failed     "; got != trimRows(want) {
		t.Errorf("rows =\n%s\nwant\n%s", got, trimRows(want))
	}
	// A task_done with no task id stays, landed event or not.
	if got := texts(evAt(1, "task_done", 0, 20, 15, 0, `{}`), evAt(2, "task_landed", 0, 20, 15, 1, `{}`)); got != "15:01  task  landed\n15:00  task  done" {
		t.Errorf("task id 0: %q", got)
	}
	// The hidden row takes no slot of the 10: 11 done tasks, then a task
	// that is done and landed.
	var many []history.Item
	for i := int64(1); i <= 11; i++ {
		many = append(many, evAt(i, "task_done", i, 20, 14, int(i), `{}`))
	}
	many = append(many, evAt(20, "task_done", 20, 20, 15, 0, `{}`), evAt(21, "task_landed", 20, 20, 15, 1, `{}`))
	rows := topEventTexts(many, api.Status{}, topNow, style, 100)[1:]
	if len(rows) != 10 || !strings.Contains(rows[0], "#20  landed") || !strings.Contains(rows[9], "#3   done") ||
		strings.Contains(strings.Join(rows, "\n"), "#20  done") {
		t.Errorf("rows = %q, want the landed task and done 11 down to 3", rows)
	}
	// Only the rendering filters: the model keeps every item.
	if model := busyTopModel(); len(model.events) != 2 {
		t.Errorf("model.events = %+v, want both items kept", model.events)
	}
}

// trimRows trims the trailing spaces of each row of text.
func trimRows(text string) string {
	rows := strings.Split(text, "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " ")
	}
	return strings.Join(rows, "\n")
}

// TestTopEventsNoTrailingSpaces: a blank number and a blank title leave no
// trailing space, and a number column blank on every row takes no width and
// no gap.
func TestTopEventsNoTrailingSpaces(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	items := []history.Item{
		evAt(1, "task_landed", 3, 20, 15, 0, `{}`),
		evAt(2, "review_verdict", 0, 20, 15, 1, `{}`),
		evAt(3, "plan_done", 0, 20, 15, 2, `{}`),
	}
	want := []string{
		"EVENTS",
		"15:02  plan        done",
		"15:01  review      no verdict",
		"15:00  task    #3  landed      oil the gears",
	}
	got := topEventTexts(items, topTitleStatus("p", "oil the gears"), topNow, style, 100)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	agents := []history.Item{
		evAt(1, "agent_broken", 0, 20, 15, 0, `{"harness":"codex","model":"gpt-5"}`),
		evAt(2, "review_verdict", 0, 20, 15, 1, `{"status":"approve","branch":"fix/x"}`),
	}
	want = []string{"EVENTS", "15:01  review  approve  fix/x", "15:00  agent   broken   codex/gpt-5"}
	if got := topEventTexts(agents, api.Status{}, topNow, style, 100); !reflect.DeepEqual(got, want) {
		t.Errorf("all-blank number column =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTopEventsWidths: no row is wider than the box at 60, 80 and 100, a
// wide-character title and a long one included, and at 60 a date row, a
// `landed, push waits` and a long title cut only the title.
func TestTopEventsWidths(t *testing.T) {
	style := term.New(logo.Caps{UTF8: true})
	status := api.Status{Plans: []engine.StatusPlan{{ID: 107, Title: "日本語日本語日本語日本語日本語日本語", Tasks: []engine.StatusTask{
		{ID: 355, Title: "CHANGELOG entry for #181 and more besides"},
		{ID: 353, Title: "日本語日本語日本語日本語"},
	}}}}
	items := []history.Item{
		evAt(1, "task_landed", 355, 19, 15, 6, `{"pushed":"waiting"}`),
		evAt(2, "task_landed", 353, 20, 14, 55, `{}`),
		evAt(3, "plan_done", 0, 20, 15, 6, `{"plan":107}`),
	}
	for _, w := range []int{60, 80, 100} {
		for _, row := range topEventTexts(items, status, topNow, style, w)[1:] {
			if n := term.CellWidth(row); n > w {
				t.Errorf("width %d: row %q is %d cells", w, row, n)
			}
		}
	}
	// The fixed columns are 43 cells, so the title has 15.
	got := topEventTexts(items[:1], status, topNow, style, 60)[1]
	if want := "10-19 15:06  task  #355  landed, push waits  CHANGELOG en..."; got != want {
		t.Errorf("width 60 = %q, want %q", got, want)
	}
	// A wide title is cut on a cell boundary: six characters and the mark.
	got = topEventTexts(items[2:], status, topNow, style, 40)[1]
	if want := "15:06  plan  #107  done  日本語日本語..."; got != want {
		t.Errorf("wide title = %q, want %q", got, want)
	}
}

// TestTopEventsStyles: ASCII and no-colour rows hold no escape and the same
// columns, a coloured row aligns with a plain one, and a `failed` in a
// title stays uncoloured.
func TestTopEventsStyles(t *testing.T) {
	status := topTitleStatus("p", "fix failed login")
	items := []history.Item{
		evAt(1, "task_landed", 3, 20, 15, 0, `{}`),
		evAt(2, "task_failed", 3, 20, 15, 1, `{}`),
		evAt(3, "agent_broken", 0, 20, 15, 2, `{"harness":"codex","model":"gpt-5"}`),
	}
	plain := topEventTexts(items, status, topNow, term.New(logo.Caps{UTF8: true}), 100)
	for name, caps := range map[string]logo.Caps{"ASCII": {}, "NO_COLOR": {UTF8: true, Color: logo.NoColor}} {
		got := topEventTexts(items, status, topNow, term.New(caps), 100)
		if !reflect.DeepEqual(got, plain) {
			t.Errorf("%s rows =\n%s\nplain\n%s", name, strings.Join(got, "\n"), strings.Join(plain, "\n"))
		}
	}
	if strings.Contains(strings.Join(plain, "\n"), "\x1b") {
		t.Error("plain rows hold an escape")
	}
	style := term.New(logo.Caps{Color: logo.Color16, UTF8: true})
	coloured := topEventTexts(items, status, topNow, style, 100)
	escape := regexp.MustCompile("\x1b\\[[0-9;]*m")
	for i, row := range coloured {
		if got := escape.ReplaceAllString(row, ""); got != plain[i] {
			t.Errorf("coloured row %q reads %q, want %q", row, got, plain[i])
		}
	}
	if !strings.Contains(coloured[2], style.Bad("failed")+"  fix failed login") {
		t.Errorf("failed row = %q, want the state coloured and the title plain", coloured[2])
	}
}
