package history

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// readableTimeLayout is the time every readable line starts with.
const readableTimeLayout = "01-02 15:04"

// RenderReadable writes a task's or a job's timeline for a person, one item
// per line (oldest first as given), each starting with its time in loc as
// `01-02 15:04`. A stamp that does not parse shows as it is. Every item
// shows: a worded one as its words, then its payload text; any other as its
// subject, its kind in plain words and its text, a worker result as its
// status and summary instead of JSON. A text runs whole, never cut; a body
// over several lines keeps indentBody's shape. The bare view of `brigada
// history` is the table in table.go; Render stays the raw rendering.
func RenderReadable(w io.Writer, items []Item, loc *time.Location) error {
	clock := func(stamp string) string { return readableTime(stamp, loc) }
	for _, it := range items {
		line := readableTime(it.CreatedAt, loc) + " " + readableDetail(it, clock)
		if _, err := fmt.Fprintln(w, indentBody(line)); err != nil {
			return err
		}
	}
	return nil
}

// readableTime is a stamp as `01-02 15:04` in loc, or as it is when it
// does not parse.
func readableTime(stamp string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return stamp
	}
	return t.In(loc).Format(readableTimeLayout)
}

// readableWords is EventWords with a review verdict's job in front, since
// a verdict is read by its job.
func readableWords(it Item, clock func(string) string) (string, bool) {
	words, ok := EventWords(it, clock)
	if ok && it.Kind == "review_verdict" && it.JobID != 0 {
		words = "job " + strconv.FormatInt(it.JobID, 10) + " " + words
	}
	return words, ok
}

// readableDetail is an item's line in a task's or a job's timeline.
func readableDetail(it Item, clock func(string) string) string {
	text := readableText(it)
	if words, ok := readableWords(it, clock); ok {
		if text == "" {
			return words
		}
		return words + ": " + text
	}
	head := subject(it) + " " + strings.ReplaceAll(it.Kind, "_", " ")
	if text == "" {
		return head
	}
	return head + ": " + text
}

// readableText is what an item says beyond its words: its summary, with
// the question and the findings an event carries, and a worker result's
// fields in place of its JSON. Nothing is cut.
func readableText(it Item) string {
	if it.Type == TypeMessage {
		if it.Kind == "worker_result" {
			if text, ok := workerResultText(it.Content); ok {
				return text
			}
		}
		return it.Content
	}
	text := summary(it)
	var p struct {
		Question string `json:"question"`
		Findings string `json:"findings"`
	}
	if json.Unmarshal(it.Payload, &p) != nil {
		return text
	}
	for _, f := range [][2]string{{"question", p.Question}, {"findings", p.Findings}} {
		if f[1] == "" {
			continue
		}
		if text != "" {
			text += " "
		}
		text += f[0] + "=" + f[1]
	}
	return text
}

// workerResultText is a worker_result message's content as its fields, one
// per line, or false when the content is not a result's JSON.
func workerResultText(content string) (string, bool) {
	var r struct {
		Status        string `json:"status"`
		Summary       string `json:"summary"`
		Question      string `json:"question"`
		Reason        string `json:"reason"`
		Discoveries   string `json:"discoveries"`
		CommitMessage string `json:"commit_message"`
	}
	if json.Unmarshal([]byte(content), &r) != nil || r.Status == "" {
		return "", false
	}
	var lines []string
	for _, f := range [][2]string{{"status", r.Status}, {"summary", r.Summary}, {"question", r.Question},
		{"reason", r.Reason}, {"discoveries", r.Discoveries}, {"commit_message", r.CommitMessage}} {
		if f[1] != "" {
			lines = append(lines, f[0]+"="+f[1])
		}
	}
	return strings.Join(lines, "\n"), true
}
