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
func summary(it Item) string { return summaryOf(it, true) }

// summaryOf is summary; withWork says whether a stopped run's task_failed
// names its branch and size, which the readable views leave to their words.
func summaryOf(it Item, withWork bool) string {
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
		Files      int64  `json:"files_changed"`
		Added      int64  `json:"lines_added"`
		Removed    int64  `json:"lines_removed"`
		Commit     string `json:"commit"`
		Pushed     string `json:"pushed"`
		PushCmd    string `json:"push_cmd"`
		AfterLand  string `json:"after_land_error"`
		TierAsked  string `json:"tier_asked"`
		TierServed string `json:"tier_served"`
		EffortFrom string `json:"effort_from"`
		EffortTo   string `json:"effort_to"`
		Agent      int64  `json:"agent"`
		Placement  string `json:"placement"`
		Layer      string `json:"layer"`
		Key        string `json:"key"`
		Prompt     *struct {
			Template     int      `json:"template"`
			Instructions string   `json:"instructions"`
			Reviewer     string   `json:"reviewer"`
			Skills       []string `json:"skills"`
		} `json:"prompt"`
		ToolGrants []string `json:"tool_grants"`
		Note       string   `json:"note"`
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
	if it.Kind == "job_agent_picked" {
		return pickedSummary(it.Payload)
	}
	if it.Kind == "job_cap_held" {
		return capHeldSummary(p.Layer, p.Key)
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
	// job with no task row to say where it reads. A landing's events —
	// task_landed, task_pushed — name their target branch too, and what
	// the gate made of the push: the resulting commit, the push state,
	// the exact push command of gate_push never, and an after_land
	// failure, reported and never undone (§6.3, §7.1).
	if it.Kind == "job_created" || it.Kind == "review_verdict" ||
		it.Kind == "task_landed" || it.Kind == "task_pushed" {
		add("branch", p.Branch)
	}
	add("answer", p.Answer)
	add("status", p.Status)
	if it.Kind == "task_landed" || it.Kind == "task_pushed" {
		add("commit", p.Commit)
		add("pushed", p.Pushed)
		add("push_cmd", p.PushCmd)
		add("after_land_error", p.AfterLand)
	}
	add("reason", p.Reason)
	// A time-capped or stalled run's task_failed names the branch its
	// work sits on and that branch's size against the task's base
	// (§5.6): what the foreman weighs a continue restart by.
	if it.Kind == "task_failed" {
		if withWork && p.Branch != "" {
			parts = append(parts, "work="+StoppedWork(p.Branch, p.Files, p.Added, p.Removed))
		}
	}
	if it.Kind == "task_note_set" {
		if p.Note == "" {
			p.Note = "(cleared)"
		}
		add("note", p.Note)
	}
	add("report", p.Report)
	add("lease", p.Lease)
	addInt("owner", p.Owner)
	addInt("run", p.Run)
	addInt("attempt", p.Attempt)
	addInt("epoch", p.Epoch)
	add("path", p.Path)
	addInt("bytes", p.Bytes)
	if len(p.ToolGrants) > 0 {
		add("tool_grants", strings.Join(p.ToolGrants, ","))
	}
	// A leased job's prompt names what it was composed from (§9): the
	// template version and the digests, shortened to the usual 12.
	if p.Prompt != nil {
		short := func(d string) string {
			if len(d) > 12 {
				return d[:12]
			}
			return d
		}
		addInt("template", int64(p.Prompt.Template))
		add("instructions", short(p.Prompt.Instructions))
		add("reviewer", short(p.Prompt.Reviewer))
		skills := make([]string, len(p.Prompt.Skills))
		for i, d := range p.Prompt.Skills {
			skills[i] = short(d)
		}
		add("skills", strings.Join(skills, ","))
	}
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

// pickedPlacement is a review's job_agent_picked line (issue #126): the
// reviewer's agent and how it relates to the author, the placement kind
// the lease recorded. Only a review's pick carries one; another job's
// pick renders as before.
func pickedPlacement(agent int64, placement string) string {
	who := "reviewer agent " + strconv.FormatInt(agent, 10)
	switch placement {
	case "cross_family":
		return who + " (cross_family: another family than the author)"
	case "same_family":
		return who + " (same_family: same family as the author, review.family=prefer)"
	case "same_agent":
		return who + " (same_agent: the author's own agent, review.family=prefer)"
	case "unknown":
		return who + " (unknown: the author's family unknown)"
	}
	return who + " (placement " + placement + ")"
}

// pickedPayload is the decision a job_agent_picked event records (§8):
// why the lease took its agent.
type pickedPayload struct {
	Agent         int64   `json:"agent"`
	Score         float64 `json:"score"`
	Pick          string  `json:"pick"`
	Weight        *int    `json:"weight"`
	Draw          float64 `json:"draw"`
	TierAsked     string  `json:"tier_asked"`
	TierServed    string  `json:"tier_served"`
	Placement     string  `json:"placement"`
	FamilyPolicy  string  `json:"family_policy"`
	Fallback      string  `json:"fallback"`
	Pin           int64   `json:"pin"`
	Session       bool    `json:"session"`
	ExcludeAgents []int64 `json:"exclude_agents"`
	Removed       []struct {
		Agent  int64  `json:"agent"`
		Reason string `json:"reason"`
	} `json:"removed"`
}

// pickedSummary words a job_agent_picked event for the timeline (§8): who
// was picked, its score, cost and draw, the tier asked and served, the
// placement and any fallback, and the agents the placement dropped with
// the reason for each. It is the one rendering `brigada history` and the
// MCP bridge's history share, so the two read the same decision.
func pickedSummary(payload []byte) string {
	var p pickedPayload
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	var parts []string
	if p.Placement != "" {
		parts = append(parts, pickedPlacement(p.Agent, p.Placement))
	} else {
		parts = append(parts, "agent "+strconv.FormatInt(p.Agent, 10))
	}
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("pick", p.Pick)
	if p.Score != 0 {
		add("score", strconv.FormatFloat(p.Score, 'f', -1, 64))
	}
	if p.Weight != nil {
		add("weight", strconv.Itoa(*p.Weight))
	}
	if p.Draw != 0 {
		add("draw", strconv.FormatFloat(p.Draw, 'f', -1, 64))
	}
	if p.TierAsked != "" || p.TierServed != "" {
		tier := p.TierAsked
		if p.TierServed != "" && p.TierServed != p.TierAsked {
			tier += "->" + p.TierServed
		}
		add("tier", tier)
	}
	add("family", p.FamilyPolicy)
	add("fallback", p.Fallback)
	if p.Pin != 0 {
		add("pin", strconv.FormatInt(p.Pin, 10))
	}
	if p.Session {
		parts = append(parts, "session")
	}
	if len(p.ExcludeAgents) > 0 {
		ids := make([]string, len(p.ExcludeAgents))
		for i, id := range p.ExcludeAgents {
			ids[i] = strconv.FormatInt(id, 10)
		}
		add("excluded", strings.Join(ids, ","))
	}
	if len(p.Removed) > 0 {
		rem := make([]string, len(p.Removed))
		for i, r := range p.Removed {
			rem[i] = strconv.FormatInt(r.Agent, 10) + ": " + r.Reason
		}
		add("removed", strings.Join(rem, ", "))
	}
	return strings.Join(parts, " ")
}

// capHeldSummary words a job_cap_held event for the timeline (§8.1): the
// layer that had no room and, when the layer names an instance, that
// instance — the account label, model name, plan id or runner id.
func capHeldSummary(layer, key string) string {
	if layer == "" {
		return ""
	}
	s := "held: " + layer
	if key != "" {
		s += " on " + key
	}
	return s
}
