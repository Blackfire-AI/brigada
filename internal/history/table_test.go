package history

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
)

func tableEvent(stamp, kind string, task, job int64, payload string) Item {
	it := readableEvent(kind, task, job, payload)
	it.CreatedAt = stamp
	return it
}

func tableRows(t *testing.T, items []Item, titles Titles) []TableRow {
	t.Helper()
	var rows []TableRow
	for _, it := range items {
		if row, ok := EventRow(it, time.UTC, titles); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

func tableText(t *testing.T, rows []TableRow, w TableWidths, style term.Style) string {
	t.Helper()
	var b bytes.Buffer
	if err := WriteTable(&b, rows, w, style); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

var tableTitles = Titles{
	Plans: map[int64]string{107: "top keeps its last column and redraws on resize"},
	Tasks: map[int64]string{353: "top: keep the last column and redraw on resize", 356: "top: EVENTS rows\nread as a table"},
}

func TestTableGolden(t *testing.T) {
	items := []Item{
		tableEvent("2026-11-05T14:55:00.000000000Z", "task_landed", 353, 0, `{"commit":"e899618aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		tableEvent("2026-11-05T15:06:00.000000000Z", "plan_done", 356, 0, `{"plan":107}`),
		tableEvent("2026-11-05T15:06:30.000000000Z", "foreman_report", 0, 2925, `{"report":"x"}`),
		tableEvent("2026-11-05T15:46:00.000000000Z", "task_done", 356, 0, `{}`),
		tableEvent("2026-11-05T15:51:00.000000000Z", "task_landed", 356, 0, `{"commit":"e357f12bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`),
		{Type: TypeMessage, ID: 9, Kind: "worker_result", Content: "noise", CreatedAt: "2026-11-05T15:52:00.000000000Z"},
	}
	rows := tableRows(t, items, tableTitles)
	want := "11-05 14:55  task    #353   landed  top: keep the last column and redraw on resize  e899618\n" +
		"11-05 15:06  plan    #107   done    top keeps its last column and redraws on resize\n" +
		"11-05 15:06  report  #2925\n" +
		"11-05 15:46  task    #356   done    top: EVENTS rows read as a table\n" +
		"11-05 15:51  task    #356   landed  top: EVENTS rows read as a table                e357f12\n"
	if got := tableText(t, rows, FitWidths(rows), term.Style{}); got != want {
		t.Errorf("table =\n%s\nwant\n%s", got, want)
	}
}

func TestTableShortCommit(t *testing.T) {
	sha := "9f21ab3c4d5e6f708192a3b4c5d6e7f890123456"
	rows := tableRows(t, []Item{
		readableEvent("task_landed", 3, 0, `{"commit":"`+sha+`","pushed":"pushed"}`),
		readableEvent("task_landed", 3, 0, `{"commit":"`+sha+`","pushed":"waiting"}`),
		readableEvent("task_pushed", 3, 0, `{"commit":"`+sha+`"}`),
		readableEvent("task_landed", 3, 0, `{"commit":"abc"}`),
		readableEvent("task_landed", 3, 0, `{}`),
		readableEvent("task_done", 3, 0, `{"commit":"`+sha+`"}`),
	}, Titles{Tasks: map[int64]string{3: "t"}})
	want := "10-20 15:02  task  #3  landed              t  9f21ab3\n" +
		"10-20 15:02  task  #3  landed, push waits  t  9f21ab3\n" +
		"10-20 15:02  task  #3  pushed              t  9f21ab3\n" +
		"10-20 15:02  task  #3  landed              t  abc\n" +
		"10-20 15:02  task  #3  landed              t\n" +
		"10-20 15:02  task  #3  done                t\n"
	if got := tableText(t, rows, FitWidths(rows), term.Style{}); got != want {
		t.Errorf("table =\n%s\nwant\n%s", got, want)
	}
}

func TestTableBlankNumberAndTitle(t *testing.T) {
	rows := tableRows(t, []Item{
		readableEvent("plan_done", 0, 0, `{}`),
		readableEvent("task_landed", 5, 0, `{"commit":"abcdef0123"}`),
		readableEvent("task_landed", 6, 0, `{"commit":"1234567890"}`),
	}, Titles{Tasks: map[int64]string{6: "six"}})
	want := "10-20 15:02  plan      done\n" +
		"10-20 15:02  task  #5  landed       abcdef0\n" +
		"10-20 15:02  task  #6  landed  six  1234567\n"
	if got := tableText(t, rows, FitWidths(rows), term.Style{}); got != want {
		t.Errorf("table =\n%s\nwant\n%s", got, want)
	}
}

func TestTableBranchReview(t *testing.T) {
	rows := tableRows(t, []Item{
		readableEvent("review_verdict", 0, 2900, `{"status":"approve","branch":"fix/x"}`),
		readableEvent("review_verdict", 0, 2901, `{"status":"changes"}`),
		readableEvent("review_verdict", 0, 0, `{"status":"changes","branch":"fix/y"}`),
		readableEvent("review_verdict", 4, 2902, `{"status":"approve","branch":"task/4"}`),
		readableEvent("task_landed", 4, 0, `{"commit":"abcdef0123"}`),
	}, Titles{Tasks: map[int64]string{4: "four"}})
	got := tableText(t, rows, FitWidths(rows), term.Style{})
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("table =\n%s", got)
	}
	for i, wantPart := range []string{"approve  fix/x · job 2900", "changes  job 2901", "changes  fix/y", "approve  four", "landed   four  abcdef0"} {
		if !strings.HasSuffix(lines[i], wantPart) {
			t.Errorf("line %d = %q, want it to end %q", i, lines[i], wantPart)
		}
	}
	if rows[0].Number != "" || rows[1].Number != "" || rows[2].Number != "" {
		t.Errorf("a branch review has a number: %+v", rows[:3])
	}
	if strings.Contains(lines[3], "job") || rows[3].Number != "#4" {
		t.Errorf("a review with a task id shows a job: %q", lines[3])
	}
	// the branch-review titles do not widen the commit column
	if !strings.HasSuffix(lines[4], "landed   four  abcdef0") {
		t.Errorf("commit column moved: %q", lines[4])
	}
}

func TestTableAfterLand(t *testing.T) {
	colour := term.New(logo.Caps{Color: logo.Color256, UTF8: true})
	for _, kind := range []string{"task_landed", "task_pushed"} {
		failed := tableRows(t, []Item{readableEvent(kind, 7, 0, `{"commit":"abcdef0123","after_land_error":"exit 3: boom","push_cmd":"git push"}`)}, Titles{Tasks: map[int64]string{7: "seven"}})
		clean := tableRows(t, []Item{readableEvent(kind, 7, 0, `{"commit":"abcdef0123"}`)}, Titles{Tasks: map[int64]string{7: "seven"}})
		w := FitWidths(failed)
		base := failed[0].Line(w, term.Style{})
		tail := "after-land failed: --task 7"
		if !strings.HasSuffix(base, "seven  abcdef0  "+tail) || strings.Contains(base, "\x1b") || strings.Contains(base, "boom") || strings.Contains(base, "git push") {
			t.Errorf("%s plain line = %q", kind, base)
		}
		if got, want := failed[0].Line(w, colour), strings.TrimSuffix(base, tail)+colour.Bad(tail); got != want || !strings.Contains(got, "\x1b") {
			t.Errorf("%s coloured line = %q, want %q", kind, got, want)
		}
		if got := clean[0].Line(FitWidths(clean), colour); strings.Contains(got, "after-land") || strings.Contains(got, "\x1b") {
			t.Errorf("%s clean line has a tail: %q", kind, got)
		}
	}
	// no commit: one gap after the title; the tail never widens a column
	rows := tableRows(t, []Item{
		readableEvent("task_landed", 7, 0, `{"after_land_error":"x"}`),
		readableEvent("task_landed", 8, 0, `{"commit":"abcdef0"}`),
	}, Titles{Tasks: map[int64]string{7: "seven", 8: "e"}})
	want := "10-20 15:02  task  #7  landed  seven  after-land failed: --task 7\n" +
		"10-20 15:02  task  #8  landed  e  abcdef0\n"
	if got := tableText(t, rows, FitWidths(rows), term.Style{}); got != want {
		t.Errorf("table =\n%s\nwant\n%s", got, want)
	}
}

func TestTableStatusWidths(t *testing.T) {
	w := StatusWidths(Titles{Plans: map[int64]string{1234: "p"}, Tasks: map[int64]string{7: "a  b\nc", 20: "xy"}})
	if w.Kind != 6 || w.What != 18 || w.Number != 5 || w.Title != 5 || w.Time != 11 {
		t.Errorf("widths = %+v", w)
	}
	// A commit aligns with no look-ahead; a wide cell shifts its row only.
	rows := tableRows(t, []Item{
		readableEvent("task_landed", 7, 0, `{"commit":"abcdef0"}`),
		readableEvent("task_landed", 123456, 0, `{"commit":"abcdef0"}`),
		readableEvent("foreman_report", 0, 2925, `{}`),
		readableEvent("task_landed", 20, 0, `{"commit":"abcdef0"}`),
	}, Titles{Tasks: map[int64]string{7: "a b c", 20: "xy", 123456: "a much longer title"}})
	want := "10-20 15:02  task    #7     landed              a b c  abcdef0\n" +
		"10-20 15:02  task    #123456  landed              a much longer title  abcdef0\n" +
		"10-20 15:02  report  #2925\n" +
		"10-20 15:02  task    #20    landed              xy     abcdef0\n"
	if got := tableText(t, rows, w, term.Style{}); got != want {
		t.Errorf("table =\n%s\nwant\n%s", got, want)
	}
}
