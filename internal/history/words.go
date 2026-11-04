package history

import (
	"encoding/json"
	"strconv"
)

// eventPayload is the part of an event's payload EventWords reads.
type eventPayload struct {
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
// does. `brigada top` and `brigada history` share it, so one event reads
// the same in both. The words name their subject (`#335 landed`,
// `plan #101 done`). clock turns a stored stamp into the clock time an
// agent_cooling's deadline shows; nil shows the stamp as it is.
func EventWords(it Item, clock func(stamp string) string) (string, bool) {
	if it.Type != TypeEvent {
		return "", false
	}
	var p eventPayload
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
			until := p.Until
			if clock != nil {
				until = clock(p.Until)
			}
			words += " until " + until
		}
		return words, true
	case "foreman_report":
		return "report #" + strconv.FormatInt(it.JobID, 10), true
	}
	return "", false
}
