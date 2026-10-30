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
// brackets only once it has one, and a done implement task carries its
// landing state beside the state — `landed@<sha>` or `not landed`. The plans list the engine trimmed
// says so in one summary line (§10): the older finished plans counted,
// the all view (`--all`, all=true) listing them.
func StatusSections(st Status) string {
	lines := runningLines(st.Running, time.Now())
	// A restart drain pending (§5.1): the runner finishes the running
	// jobs above and exits, and its restart waits behind them.
	if st.RunnerDraining && len(st.Running) > 0 {
		lines = append(lines, drainLine(len(st.Running)))
	}
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
	for _, r := range st.Retries {
		lines = append(lines, "waiting: "+RetryLine(r, time.Now()))
	}
	if len(st.Plans) == 0 {
		lines = append(lines, "plans: none")
	} else {
		lines = append(lines, "plans:")
		for _, p := range st.Plans {
			// A reviewing plan names its round (§6.2): `plan 12
			// reviewing (round 1)`; a plan its review parked for the
			// foreman names its round the same way.
			state := p.State
			if state == "reviewing" || state == "needs_foreman" {
				state = fmt.Sprintf("%s (round %d)", state, p.ReviewRound)
			}
			lines = append(lines, fmt.Sprintf("#%d %s %s", p.ID, state, p.Title))
			// The review's latest verdict and findings, so the foreman
			// reads what the reviewer said through the status tool. The
			// findings are the event's whole text; the line cuts at its
			// own bound and, when the cut landed, points at the review
			// job's history, which holds them whole (§6.2).
			if p.ReviewVerdict != "" || p.ReviewFindings != "" {
				review := "  review: " + p.ReviewVerdict
				if p.ReviewFindings != "" {
					review += ", findings: " + findingsLine(p.ReviewFindings, p.ReviewJob)
				}
				lines = append(lines, review)
			}
			// Waiting amendment work (§6.2): tasks an amendment added
			// or respecified under the running round, pending the
			// verdict that queues the next round over them — never
			// invisible in the status.
			if p.WaitingAmendment {
				lines = append(lines, "  amendment waits for the review round")
			}
			for _, t := range p.Tasks {
				line := fmt.Sprintf("  task #%d %s %s", t.ID, t.State, t.Title)
				// A done implement task carries its landing state (§7.1):
				// `landed@<sha>` or `not landed`, so a task done and not
				// landed reads differently from a landed one.
				if label := t.LandedLabel(); label != "" {
					line += " " + label
				}
				if t.LandReady != "" {
					line += " " + t.LandReady
				}
				if len(t.WaitingOn) > 0 {
					line += " (waiting on " + strings.Join(t.WaitingOn, ", ") + ")"
				}
				if t.Continues != "" {
					line += " (continues " + t.Continues + ")"
				}
				if len(t.Skills) > 0 {
					line += " skills:" + strings.Join(t.Skills, ",")
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

	for _, in := range st.RoleInstructions {
		line := fmt.Sprintf("%s instructions (project #%d): %s", in.Role, in.ProjectID, in.State)
		if in.State == "file" {
			line += fmt.Sprintf(", %d bytes, sha256 %s", in.Bytes, in.Digest)
		}
		if in.GeneratedFrom != "" {
			line += ", generated from " + in.GeneratedFrom
		}
		if in.Edited != nil {
			line += fmt.Sprintf(", edited %t", *in.Edited)
		}
		if in.ChangedSinceImport != nil {
			line += fmt.Sprintf(", changed since import %t", *in.ChangedSinceImport)
		}
		lines = append(lines, line)
	}
	for _, skill := range st.Skills {
		line := fmt.Sprintf("skill %s (project #%d): %s, sha256 %s, used %d times", skill.Name, skill.ProjectID, skill.Description, skill.Digest, skill.TimesUsed)
		if skill.ChangedSinceImport != nil {
			line += fmt.Sprintf(", changed since import %t", *skill.ChangedSinceImport)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// RetryLine words one job waiting on an infra retry (§5.1) for the
// status and top views: "task #4 work: retry in 5m: could not resolve
// github.com" — the job or task, its kind, the time left to its
// not-before and the failure's words.
func RetryLine(r engine.InfraRetry, now time.Time) string {
	what := fmt.Sprintf("job #%d", r.ID)
	if r.TaskID != 0 {
		what = fmt.Sprintf("task #%d", r.TaskID)
	}
	var left time.Duration
	if t, err := time.Parse(engine.TimeLayout, r.NotBefore); err == nil && t.After(now) {
		left = t.Sub(now)
	}
	// Whole minutes round up, so a wait never reads shorter than it is.
	wait := left.Truncate(time.Second).String()
	if left >= time.Minute {
		wait = fmt.Sprintf("%dm", int((left+time.Minute-1)/time.Minute))
	}
	line := fmt.Sprintf("%s %s: retry in %s", what, r.Kind, wait)
	if r.Reason != "" {
		line += ": " + r.Reason
	}
	return line
}

// shortVersion is the first 12 characters of an import hash.
func shortVersion(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// findingsLine renders a plan review's findings for the status's plan
// line: one line of at most about 200 bytes, and — when the findings
// are longer and their review job is known — ending with the pointer to
// the history that holds them whole (§6.2, §9).
func findingsLine(findings string, job int64) string {
	line := oneLine(findings, 200)
	if flat := strings.Join(strings.Fields(findings), " "); job > 0 && len(flat) > 200 {
		line += fmt.Sprintf("… (full: brigada history --job %d)", job)
	}
	return line
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

// drainLine is the status's word for a pending restart drain (§5.1):
// the runner restarts once the running jobs it waits out have finished.
func drainLine(n int) string {
	noun := "jobs"
	if n == 1 {
		noun = "job"
	}
	return fmt.Sprintf("runner restarting after %d running %s", n, noun)
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
