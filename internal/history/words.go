package history

import (
	"encoding/json"
	"strconv"
)

// wordsPayload is the part of an event's payload the plain words use.
type wordsPayload struct {
	Plan    int64  `json:"plan"`
	Status  string `json:"status"`
	Branch  string `json:"branch"`
	Pushed  string `json:"pushed"`
	Harness string `json:"harness"`
	Model   string `json:"model"`
	Account string `json:"account"`
	Until   string `json:"until"`
}

// EventWords is an event in plain words, or false when its kind is not on
// the allowlist below: only the allowlist has words, and a message never
// does. clock turns a stored stamp into the clock time the words show for
// an agent_cooling's deadline; the caller picks the zone and the form.
// `brigada top` and `brigada history` word an event through this one
// function.
func EventWords(it Item, clock func(stamp string) string) (string, bool) {
	if it.Type != TypeEvent {
		return "", false
	}
	var p wordsPayload
	if len(it.Payload) > 0 {
		_ = json.Unmarshal(it.Payload, &p) // an unreadable payload words as an empty one
	}
	task := "task"
	if it.TaskID != 0 {
		task = "#" + strconv.FormatInt(it.TaskID, 10)
	}
	plan := "plan"
	if p.Plan != 0 {
		plan = "plan #" + strconv.FormatInt(p.Plan, 10)
	}
	agent := p.Harness + "/" + p.Model
	switch it.Kind {
	case "plan_done":
		return plan + " done", true
	case "plan_approved":
		return plan + " approved", true
	case "plan_awaiting_approval":
		return plan + " awaiting approval", true
	case "task_done":
		return task + " done", true
	case "task_landed":
		if p.Pushed == "waiting" || p.Pushed == "local" {
			return task + " landed, push waits", true
		}
		return task + " landed", true
	case "task_pushed":
		return task + " pushed", true
	case "task_failed":
		return task + " failed", true
	case "task_land_failed":
		return task + " land failed", true
	case "review_verdict":
		verdict := "review no verdict"
		switch p.Status {
		case "approve":
			verdict = "review approve"
		case "changes":
			verdict = "review changes"
		}
		if p.Branch != "" {
			verdict += " " + p.Branch
		}
		return verdict, true
	case "agent_broken":
		return agent + " broken", true
	case "agent_recovered":
		return agent + " recovered", true
	case "agent_cooling":
		words := p.Account + " cooling"
		if p.Until != "" {
			words += " until " + clock(p.Until)
		}
		return words, true
	case "foreman_report":
		return "report #" + strconv.FormatInt(it.JobID, 10), true
	}
	return "", false
}

// EventCols is an event in the columns `brigada top` reads as a table.
type EventCols struct {
	// Kind is the one-word class: plan, task, review, agent or, for any
	// other allowlisted kind, its own word.
	Kind string
	// Number is the plan's or task's number, 0 when the event has none.
	Number int64
	// What is what happened, in a few words.
	What string
	// Subject is what the event is about beyond a number: a review's
	// branch, an agent's name; empty when it has none.
	Subject string
}

// EventColumns is EventWords as columns, from the same allowlist: it says
// false for exactly the items EventWords does. clock is as in EventWords.
func EventColumns(it Item, clock func(stamp string) string) (EventCols, bool) {
	if it.Type != TypeEvent {
		return EventCols{}, false
	}
	var p wordsPayload
	if len(it.Payload) > 0 {
		_ = json.Unmarshal(it.Payload, &p) // an unreadable payload words as an empty one
	}
	task := func(what string) (EventCols, bool) {
		return EventCols{Kind: "task", Number: it.TaskID, What: what}, true
	}
	// plan_done carries the last task's id in TaskID, so a plan's number
	// is the payload's alone.
	plan := func(what string) (EventCols, bool) {
		return EventCols{Kind: "plan", Number: p.Plan, What: what}, true
	}
	switch it.Kind {
	case "plan_done":
		return plan("done")
	case "plan_approved":
		return plan("approved")
	case "plan_awaiting_approval":
		return plan("awaiting approval")
	case "task_done":
		return task("done")
	case "task_landed":
		if p.Pushed == "waiting" || p.Pushed == "local" {
			return task("landed, push waits")
		}
		return task("landed")
	case "task_pushed":
		return task("pushed")
	case "task_failed":
		return task("failed")
	case "task_land_failed":
		return task("land failed")
	case "review_verdict":
		what := "no verdict"
		switch p.Status {
		case "approve":
			what = "approve"
		case "changes":
			what = "changes"
		}
		return EventCols{Kind: "review", Number: it.TaskID, What: what, Subject: p.Branch}, true
	case "agent_broken":
		return EventCols{Kind: "agent", What: "broken", Subject: p.Harness + "/" + p.Model}, true
	case "agent_recovered":
		return EventCols{Kind: "agent", What: "recovered", Subject: p.Harness + "/" + p.Model}, true
	case "agent_cooling":
		subject := p.Account
		if p.Until != "" {
			subject += " until " + clock(p.Until)
		}
		return EventCols{Kind: "agent", What: "cooling", Subject: subject}, true
	case "foreman_report":
		return EventCols{Kind: "report", What: "#" + strconv.FormatInt(it.JobID, 10)}, true
	}
	return EventCols{}, false
}
