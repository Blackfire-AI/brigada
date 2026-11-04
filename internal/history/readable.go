package history

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

// ReadableOptions shape RenderReadable.
type ReadableOptions struct {
	// Full shows every item, internal kinds and messages included, with
	// what each one carries; otherwise only the items EventWords words
	// show, one line each (plus a landing's record).
	Full bool
	// Loc is the zone of the times; nil is time.Local.
	Loc *time.Location
	// Clock is EventWords' clock; nil shows a deadline's stamp as it is.
	Clock func(stamp string) string
}

// RenderReadable writes the timeline for a person, oldest first as the
// items come: each item starts with its local time as `01-02 15:04`, then
// the shared words (EventWords), which already name their subject. A
// review verdict names its job. A body over several lines keeps
// indentBody's shape. Render stays the byte-exact machine rendering.
func RenderReadable(w io.Writer, items []Item, o ReadableOptions) error {
	loc := o.Loc
	if loc == nil {
		loc = time.Local
	}
	for _, it := range items {
		text, ok := readableText(it, o)
		if !ok {
			continue
		}
		if _, err := io.WriteString(w, indentBody(readableTime(it.CreatedAt, loc)+"  "+text)+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// readableTime is a stored stamp as `01-02 15:04` in loc; a stamp that
// does not parse shows as it is.
func readableTime(stamp string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return stamp
	}
	return t.In(loc).Format("01-02 15:04")
}

// readableWords is the shared words, a review verdict led by its job.
func readableWords(it Item, clock func(string) string) (string, bool) {
	words, ok := EventWords(it, clock)
	if ok && it.Kind == "review_verdict" && it.JobID != 0 {
		words = "job " + strconv.FormatInt(it.JobID, 10) + " " + words
	}
	return words, ok
}

// readableText is one item's text without its time, or false when the
// view leaves the item out.
func readableText(it Item, o ReadableOptions) (string, bool) {
	words, ok := readableWords(it, o.Clock)
	if !o.Full {
		if !ok {
			return "", false
		}
		if it.Kind == "task_landed" || it.Kind == "task_pushed" {
			words += landingRecord(it)
		}
		return words, true
	}
	if it.Type == TypeMessage {
		return messageText(it), true
	}
	head := words
	if !ok {
		head = subject(it) + " " + kindWords(it.Kind)
	}
	text := head
	if s := summary(it); s != "" {
		text += ": " + s
	}
	return text + extraFields(it), true
}

// landingRecord is the lines after a landing's words: the target branch
// and the commit in full, then the exact push command and the after_land
// failure when set, each whole.
func landingRecord(it Item) string {
	var p struct {
		Branch    string `json:"branch"`
		Commit    string `json:"commit"`
		PushCmd   string `json:"push_cmd"`
		AfterLand string `json:"after_land_error"`
	}
	if json.Unmarshal(it.Payload, &p) != nil {
		return ""
	}
	var b strings.Builder
	var first []string
	if p.Branch != "" {
		first = append(first, "branch="+p.Branch)
	}
	if p.Commit != "" {
		first = append(first, "commit="+p.Commit)
	}
	if len(first) > 0 {
		b.WriteString("\n" + strings.Join(first, " "))
	}
	if p.PushCmd != "" {
		b.WriteString("\npush_cmd=" + p.PushCmd)
	}
	if p.AfterLand != "" {
		b.WriteString("\nafter_land_error=" + p.AfterLand)
	}
	return b.String()
}

// extraFields is what summary leaves out of an event and a person reads:
// the question a worker asked and a review's findings, each whole.
func extraFields(it Item) string {
	var p struct {
		Question string `json:"question"`
		Findings string `json:"findings"`
	}
	if json.Unmarshal(it.Payload, &p) != nil {
		return ""
	}
	var b strings.Builder
	if p.Question != "" {
		b.WriteString("\nquestion: " + p.Question)
	}
	if p.Findings != "" {
		b.WriteString("\nfindings: " + p.Findings)
	}
	return b.String()
}

// kindWords is an event kind in plain words: task_landed, task landed.
func kindWords(kind string) string {
	return strings.ReplaceAll(kind, "_", " ")
}

// messageText is a message for the full view: its author and kind in
// plain words, then its content whole. A worker_result shows as its
// status and summary, and the question, reason, discoveries and commit
// message when set; content that is not that JSON shows as it is.
func messageText(it Item) string {
	kw := kindWords(it.Kind)
	head := kw
	if it.Author != "" && !strings.HasPrefix(kw, it.Author+" ") {
		head = it.Author + " " + kw
	}
	if it.Kind == "worker_result" {
		var r struct {
			Status        string `json:"status"`
			Summary       string `json:"summary"`
			Question      string `json:"question"`
			Reason        string `json:"reason"`
			Discoveries   string `json:"discoveries"`
			CommitMessage string `json:"commit_message"`
		}
		if json.Unmarshal([]byte(it.Content), &r) == nil && r.Status != "" {
			text := head + ": status=" + r.Status
			for _, f := range []struct{ name, v string }{
				{"summary", r.Summary}, {"question", r.Question}, {"reason", r.Reason},
				{"discoveries", r.Discoveries}, {"commit_message", r.CommitMessage},
			} {
				if f.v != "" {
					text += "\n" + f.name + ": " + f.v
				}
			}
			return text
		}
	}
	return head + ": " + it.Content
}
