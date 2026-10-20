package history

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Render writes the timeline as plain text, one item per line:
//
//	<time> <kind> <subject>: <summary>
//
// A body that runs over several lines (a task spec, a worker result)
// keeps its shape, with every continuation line indented by two spaces
// so one item stays readable as one block. This is the rendering
// `brigada history` will sit on (§9).
func Render(w io.Writer, items []Item) error {
	for _, it := range items {
		line := strings.Join([]string{it.CreatedAt, it.Kind, subject(it)}, " ") + ": " + summary(it)
		if _, err := fmt.Fprintln(w, indentBody(line)); err != nil {
			return err
		}
	}
	return nil
}

// subject is what the item is about: the narrowest reference an event
// carries — its job, else its task, else its project — and a message's
// author, a message being someone's turn rather than a change to a row.
func subject(it Item) string {
	if it.Type == TypeMessage {
		return it.Author
	}
	switch {
	case it.JobID != 0:
		return "job " + strconv.FormatInt(it.JobID, 10)
	case it.TaskID != 0:
		return "task " + strconv.FormatInt(it.TaskID, 10)
	default:
		return "project " + strconv.FormatInt(it.ProjectID, 10)
	}
}

// summary is the one-line gist of the item: a message is its content,
// an event the fields its payload carries, as key=value. Unknown fields
// stay in the payload the JSON item shows; the rendering keeps to the
// vocabulary every engine event uses.
func summary(it Item) string {
	if it.Type == TypeMessage {
		return it.Content
	}
	var p struct {
		Answer     string `json:"answer"`
		Status     string `json:"status"`
		Reason     string `json:"reason"`
		Report     string `json:"report"`
		Lease      string `json:"lease"`
		Owner      int64  `json:"owner"`
		Run        int64  `json:"run"`
		Attempt    int64  `json:"attempt"`
		Epoch      int64  `json:"epoch"`
		Path       string `json:"path"`
		Bytes      int64  `json:"bytes"`
		Branch     string `json:"branch"`
		TierAsked  string `json:"tier_asked"`
		TierServed string `json:"tier_served"`
		EffortFrom string `json:"effort_from"`
		EffortTo   string `json:"effort_to"`
		Until      string `json:"until"`
		Count      int64  `json:"count"`
	}
	if json.Unmarshal(it.Payload, &p) != nil {
		return string(it.Payload)
	}
	if it.Kind == "task_tier_bumped" {
		return fmt.Sprintf("task tier %s -> %s: %s", p.TierAsked, p.TierServed, p.Reason)
	}
	if it.Kind == "task_effort_bumped" {
		return fmt.Sprintf("task effort %s -> %s: %s", p.EffortFrom, p.EffortTo, p.Reason)
	}
	if it.Kind == "foreman_wake_deferred" {
		// Until is when the quiet period lapses, Count the rows that
		// waited (§4.6): the record of a wake held back.
		return fmt.Sprintf("headless wake deferred until=%s waiting=%d", p.Until, p.Count)
	}
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	addInt := func(k string, v int64) {
		if v != 0 {
			parts = append(parts, k+"="+strconv.FormatInt(v, 10))
		}
	}
	// Only a standalone review's events name their branch: it is the one
	// job with no task row to say where it reads.
	if it.Kind == "job_created" || it.Kind == "review_verdict" {
		add("branch", p.Branch)
	}
	add("answer", p.Answer)
	add("status", p.Status)
	add("reason", p.Reason)
	add("report", p.Report)
	add("lease", p.Lease)
	addInt("owner", p.Owner)
	addInt("run", p.Run)
	addInt("attempt", p.Attempt)
	addInt("epoch", p.Epoch)
	add("path", p.Path)
	addInt("bytes", p.Bytes)
	return strings.Join(parts, " ")
}

// indentBody indents every line after the first by two spaces, so a
// multi-line body never blurs into the next item. A blank line stays
// blank — indentation on it would only be trailing whitespace.
func indentBody(line string) string {
	if !strings.Contains(line, "\n") {
		return line
	}
	lines := strings.Split(strings.TrimRight(line, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] != "" {
			lines[i] = "  " + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}
