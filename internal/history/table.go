package history

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/term"
)

// TableRows is how many rows the bare view of `brigada history` prints
// unless it is asked for them all.
const TableRows = 50

// tableTimeWidth is the width of `MM-DD HH:MM`.
const tableTimeWidth = len(readableTimeLayout)

// Titles are the plan and task titles a table row looks up by number.
type Titles struct {
	Plans map[int64]string
	Tasks map[int64]string
}

// TableRow is one row of the bare view: the cells of an allowlisted event.
type TableRow struct {
	Time, Kind, Number, What, Title string
	// Commit is the landing's short commit; it opens the commit column.
	Commit string
	// AfterLandFailed is set on a landing whose after_land failed; the row
	// then ends with the text AfterLandText names.
	AfterLandFailed bool
	afterLandTask   int64
}

// AfterLandText is the tail of a landing row whose after_land failed.
func (r TableRow) AfterLandText() string {
	if !r.AfterLandFailed {
		return ""
	}
	return "after-land failed: --task " + strconv.FormatInt(r.afterLandTask, 10)
}

// TableWidths are the widths the columns are padded to. A column of width 0
// holding no text on a row takes no width and no gap.
type TableWidths struct {
	Time, Kind, Number, What int
	// Title is the width of the title column on a row that has a commit; a
	// title on any other row is its last cell and is never padded.
	Title int
}

// EventRow is an item as a table row, or false when the item is not on the
// allowlist EventColumns accepts. Times show in loc.
func EventRow(it Item, loc *time.Location, titles Titles) (TableRow, bool) {
	clock := func(stamp string) string { return readableTime(stamp, loc) }
	cols, ok := EventColumns(it, clock)
	if !ok {
		return TableRow{}, false
	}
	row := TableRow{Time: readableTime(it.CreatedAt, loc), Kind: cols.Kind, What: cols.What}
	if cols.Number != 0 {
		row.Number = "#" + strconv.FormatInt(cols.Number, 10)
	}
	title := cols.Subject
	switch cols.Kind {
	case "plan":
		title = titles.Plans[cols.Number]
	case "task":
		title = titles.Tasks[cols.Number]
	case "review":
		if cols.Number != 0 {
			title = titles.Tasks[cols.Number]
		} else {
			// A branch review: its job is how its findings are opened.
			title = strings.Join(strings.Fields(cols.Subject), " ")
			if it.JobID != 0 {
				job := "job " + strconv.FormatInt(it.JobID, 10)
				if title == "" {
					title = job
				} else {
					title += " · " + job
				}
			}
		}
	case "report":
		row.Number, row.What = cols.What, ""
	}
	row.Title = strings.Join(strings.Fields(title), " ")
	if it.Kind == "task_landed" || it.Kind == "task_pushed" {
		var p struct {
			Commit    string `json:"commit"`
			AfterLand string `json:"after_land_error"`
		}
		_ = json.Unmarshal(it.Payload, &p)
		if r := []rune(p.Commit); len(r) > 7 {
			p.Commit = string(r[:7])
		}
		row.Commit = p.Commit
		if p.AfterLand != "" {
			row.AfterLandFailed, row.afterLandTask = true, it.TaskID
		}
	}
	return row, true
}

// FitWidths are the widths that fit exactly the rows given: each column the
// widest of its cells, the title the widest among rows with a commit.
func FitWidths(rows []TableRow) TableWidths {
	var w TableWidths
	for _, r := range rows {
		w.Time = max(w.Time, term.CellWidth(r.Time))
		w.Kind = max(w.Kind, term.CellWidth(r.Kind))
		w.Number = max(w.Number, term.CellWidth(r.Number))
		w.What = max(w.What, term.CellWidth(r.What))
		if r.Commit != "" {
			w.Title = max(w.Title, term.CellWidth(r.Title))
		}
	}
	return w
}

// StatusWidths are the widths known before any row is read: a time, the
// widest kind (`review`) and what (`landed, push waits`), the widest `#id`
// of a plan or a task, and the widest task title, since a commit row's title
// is always a task title.
func StatusWidths(titles Titles) TableWidths {
	w := TableWidths{Time: tableTimeWidth, Kind: len("review"), What: len("landed, push waits")}
	for id := range titles.Plans {
		w.Number = max(w.Number, len(strconv.FormatInt(id, 10))+1)
	}
	for id, title := range titles.Tasks {
		w.Number = max(w.Number, len(strconv.FormatInt(id, 10))+1)
		w.Title = max(w.Title, term.CellWidth(strings.Join(strings.Fields(title), " ")))
	}
	return w
}

// Line is the row as one line in the widths given. A cell past its width is
// never cut: it runs on and shifts its row alone. Trailing spaces go.
func (r TableRow) Line(w TableWidths, style term.Style) string {
	pad := func(cell string, width int) string {
		if n := width - term.CellWidth(cell); n > 0 {
			return cell + strings.Repeat(" ", n)
		}
		return cell
	}
	parts := []string{pad(r.Time, w.Time)}
	for _, c := range []struct {
		cell  string
		width int
	}{{r.Kind, w.Kind}, {r.Number, w.Number}, {r.What, w.What}} {
		if c.width == 0 && c.cell == "" {
			continue
		}
		parts = append(parts, pad(c.cell, c.width))
	}
	switch {
	case r.Commit != "":
		if w.Title > 0 || r.Title != "" {
			parts = append(parts, pad(r.Title, w.Title))
		}
		parts = append(parts, r.Commit)
	case r.Title != "":
		parts = append(parts, r.Title)
	}
	if r.AfterLandFailed {
		parts = append(parts, style.Bad(r.AfterLandText()))
	}
	return strings.TrimRight(strings.Join(parts, "  "), " ")
}

// WriteTable writes the rows, one line each, in the widths given.
func WriteTable(out io.Writer, rows []TableRow, w TableWidths, style term.Style) error {
	for _, r := range rows {
		if _, err := io.WriteString(out, r.Line(w, style)+"\n"); err != nil {
			return err
		}
	}
	return nil
}
