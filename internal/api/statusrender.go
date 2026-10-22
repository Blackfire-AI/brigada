package api

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// StatusSections renders one status answer's running jobs, plans,
// foreman report and "needs you" sections (§6.1, §6.2, §3, §10) as
// plain text, after the pid/uptime and jobs lines: the `running:` lines
// — one per job a runner holds, with the worker's latest event — then
// the `plans:` lines — one per plan, its
// tasks indented beneath it — then the `foreman report:` block — the
// latest report's time and first lines, the project named — then the
// `needs you:` queue, "nothing" when it is empty. The client's status
// command and the MCP bridge's status tool share this rendering, so a
// user and the foreman read the same lines; a task's branch rides in
// brackets only once it has one. The plans list the engine trimmed
// says so in one summary line (§10): the older finished plans counted,
// the all view (`--all`, all=true) listing them.
func StatusSections(st Status) string {
	lines := runningLines(st.Running, time.Now())
	for _, a := range st.Cooling {
		lines = append(lines, fmt.Sprintf("cooling: %s/%s until %s (account %s)", a.Harness, a.Model, a.Until, a.Account))
	}
	for _, sl := range st.Slots {
		if sl.State == engine.SlotFree {
			continue
		}
		line := fmt.Sprintf("foreman slot (project #%d): %s", sl.ProjectID, sl.State)
		if sl.Label != "" {
			line += fmt.Sprintf(" %q", sl.Label)
		}
		line += fmt.Sprintf(" #%d since %s", sl.HolderID, sl.Since)
		if len(sl.Events) > 0 {
			line += fmt.Sprintf(", carrying %d event(s)", len(sl.Events))
		}
		lines = append(lines, line)
	}
	for _, h := range st.Held {
		what := fmt.Sprintf("job #%d", h.ID)
		if h.TaskID != 0 {
			what = fmt.Sprintf("task #%d", h.TaskID)
		}
		if h.Layer == "no_pair" {
			lines = append(lines, fmt.Sprintf("held: %s %s: foreman turn waiting: no runner offers %s", what, h.Kind, h.Agent))
			continue
		}
		wait := "waiting for a free " + h.Layer + " slot"
		switch {
		case h.Layer == "runner":
			wait = "waiting for a runner slot"
		case h.Layer == "cooling" && h.Agent != "":
			wait = fmt.Sprintf("waiting: %s cooling until %s", h.Agent, h.Until)
		case h.Layer == "cooling":
			wait = "waiting: agent cooling"
		}
		lines = append(lines, fmt.Sprintf("held: %s %s: %s", what, h.Kind, wait))
	}
	if len(st.Plans) == 0 {
		lines = append(lines, "plans: none")
	} else {
		lines = append(lines, "plans:")
		for _, p := range st.Plans {
			// A reviewing plan names its round (§6.2): `plan 12
			// reviewing (round 1)`.
			state := p.State
			if state == "reviewing" {
				state = fmt.Sprintf("reviewing (round %d)", p.ReviewRound)
			}
			lines = append(lines, fmt.Sprintf("#%d %s %s", p.ID, state, p.Title))
			// The review's latest verdict and findings, so the foreman
			// reads what the reviewer said through the status tool.
			if p.ReviewVerdict != "" || p.ReviewFindings != "" {
				review := "  review: " + p.ReviewVerdict
				if p.ReviewFindings != "" {
					review += ", findings: " + oneLine(p.ReviewFindings, 200)
				}
				lines = append(lines, review)
			}
			for _, t := range p.Tasks {
				line := fmt.Sprintf("  task #%d %s %s", t.ID, t.State, t.Title)
				if t.Landed {
					line += " landed"
				}
				if len(t.WaitingOn) > 0 {
					line += " (waiting on " + strings.Join(t.WaitingOn, ", ") + ")"
				}
				if t.Continues != "" {
					line += " (continues " + t.Continues + ")"
				}
				if t.Branch != "" {
					line += " [" + t.Branch + "]"
				}
				tier := t.Tier
				if tier == "" {
					tier = "standard"
				}
				effective := t.EffectiveTier
				if effective == "" {
					effective = tier
				}
				var detail []string
				if effective != tier {
					detail = append(detail, tier+"->"+effective)
				} else if tier != "standard" {
					detail = append(detail, tier)
				}
				if t.EffectiveEffort != "" && t.EffectiveEffort != t.Effort {
					// An effort step of the escalation ladder (§8); an empty
					// task effort is the agent's own level.
					from := t.Effort
					if from == "" {
						from = "default"
					}
					detail = append(detail, "@"+from+"->"+t.EffectiveEffort)
				} else if t.Effort != "" {
					detail = append(detail, "@"+t.Effort)
				}
				// A task's own review tier and effort (§7, §8); the engine
				// default is left unsaid.
				if t.ReviewTier != "" {
					detail = append(detail, "review:"+t.ReviewTier)
				}
				if t.ReviewEffort != "" {
					detail = append(detail, "review@"+t.ReviewEffort)
				}
				if label := engine.TaskCapsLabel(t.Caps); label != "" {
					detail = append(detail, "("+label+")")
				}
				if len(detail) > 0 {
					line += "  " + strings.Join(detail, " ")
				}
				lines = append(lines, line)
			}
		}
	}
	// What the engine trimmed (§10): the older finished plans counted
	// in one line, the all view listing them.
	if st.HiddenFinishedPlans > 0 {
		lines = append(lines, fmt.Sprintf("%d older finished plans hidden; pass all=true", st.HiddenFinishedPlans))
	}
	if st.Report == nil {
		lines = append(lines, "foreman report: none")
	} else {
		lines = append(lines, "foreman report:")
		for i, ln := range strings.Split(st.Report.Text, "\n") {
			if i == 0 {
				ln = fmt.Sprintf("%s project #%d: %s", st.Report.CreatedAt, st.Report.ProjectID, ln)
			} else if ln != "" {
				ln = "  " + ln
			}
			lines = append(lines, ln)
		}
	}
	if st.ForemanMessages > 0 {
		lines = append(lines, fmt.Sprintf("messages for the foreman: %d waiting", st.ForemanMessages))
	}
	if len(st.NeedsYou) == 0 {
		lines = append(lines, "needs you: nothing")
	} else {
		lines = append(lines, "needs you:")
		for _, n := range st.NeedsYou {
			line := fmt.Sprintf("%s #%d %s", n.Kind, n.ID, n.State)
			if n.Kind == "project" {
				lines = append(lines, n.Reason)
				continue
			}
			if n.Kind == "agent" {
				line = fmt.Sprintf("agent %s %s", n.Title, n.State) // named as `brigada agent reset` takes it
			}
			if n.Reason != "" {
				line += ": " + n.Reason
			}
			lines = append(lines, line)
		}
	}
	for _, in := range st.Instructions {
		line := fmt.Sprintf("project instructions (project #%d): %s, %d bytes, sha256 %s", in.ProjectID, in.Source, in.Bytes, in.Digest)
		if in.Version != "" {
			line += ", version " + shortVersion(in.Version)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// shortVersion is the first 12 characters of an import hash.
func shortVersion(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// oneLine flattens s to a single line of at most about max bytes, the
// cut backing off to a complete rune.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// runningLines renders the jobs a runner holds (§10), one line each:
// the job, its task, its kind, the agent, how long it has run and what
// the worker did last.
func runningLines(jobs []engine.StatusJob, now time.Time) []string {
	if len(jobs) == 0 {
		return []string{"running: none"}
	}
	lines := []string{"running:"}
	for _, j := range jobs {
		lines = append(lines, "  "+describeJob(j, now))
	}
	return lines
}

// describeJob is one running job in a few words: "task #3 work ·
// claude/sonnet · 1m40s · Edit". A foreman turn names no task; the
// parts the engine does not know yet are left out. A "tool_start Edit"
// event shows as the tool's name alone; other events show as they are.
func describeJob(j engine.StatusJob, now time.Time) string {
	head := fmt.Sprintf("job #%d", j.ID)
	if j.TaskID != 0 {
		head = fmt.Sprintf("task #%d", j.TaskID)
	}
	parts := []string{head + " " + j.Kind}
	if j.Harness != "" {
		agent := j.Harness
		if j.Model != "" {
			agent += "/" + j.Model
			if j.Effort != "" {
				agent += "@" + j.Effort
			}
		}
		parts = append(parts, agent)
	}
	if t, err := time.Parse(engine.TimeLayout, j.LeasedAt); err == nil && !now.Before(t) {
		parts = append(parts, now.Sub(t).Truncate(time.Second).String())
	}
	if j.LastEvent != "" {
		parts = append(parts, strings.TrimPrefix(j.LastEvent, "tool_start "))
	}
	return strings.Join(parts, " · ")
}

// StatusLine renders the status as one short line for a terminal status
// bar (§10): the running jobs — the first one described, the rest
// counted — then what needs the user, or "idle" when nothing runs and
// nothing waits. An idle line names the latest plan that finished
// inside status's window, so a bar shows a plan's end even when nothing
// else announced it.
func StatusLine(st Status, now time.Time) string {
	var parts []string
	switch n := len(st.Running); {
	case n == 1:
		parts = append(parts, describeJob(st.Running[0], now))
	case n > 1:
		parts = append(parts, fmt.Sprintf("%s (+%d more)", describeJob(st.Running[0], now), n-1))
	}
	if q := st.Jobs["queued"]; q > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", q))
	}
	switch n := st.ForemanMessages; {
	case n == 1:
		parts = append(parts, "1 message for the foreman")
	case n > 1:
		parts = append(parts, fmt.Sprintf("%d messages for the foreman", n))
	}
	if n := len(st.NeedsYou); n > 0 {
		parts = append(parts, fmt.Sprintf("needs you: %d", n))
	}
	if len(parts) == 0 {
		if p := latestDonePlan(st.Plans); p != nil {
			return fmt.Sprintf("brigada: idle · plan #%d done", p.ID)
		}
		return "brigada: idle"
	}
	return "brigada: " + strings.Join(parts, " · ")
}

// latestDonePlan is the done plan with the highest id among those the
// status lists, or nil: plan ids grow with time, and status lists only
// the plans that finished inside its window.
func latestDonePlan(plans []engine.StatusPlan) *engine.StatusPlan {
	var latest *engine.StatusPlan
	for i := range plans {
		if plans[i].State == "done" && (latest == nil || plans[i].ID > latest.ID) {
			latest = &plans[i]
		}
	}
	return latest
}
