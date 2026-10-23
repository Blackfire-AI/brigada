package notify

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// The event kinds the engine's log can carry that wake the user (§10).
// The set is the notification vocabulary: a plan finishing, the
// foreman's report after a headless turn, the foreman's limit and its
// budget out (§4.4, §4.3), and the reasons a task parks at
// needs_foreman — the same kinds the "needs you" queue reads in
// `brigada status`. Everything else in the log never notifies.
const (
	KindPlanDone               = "plan_done"                 // the plan's last open task finished (§6.2)
	KindTaskCheckFailed        = "task_check_failed"         // the check failed; the task waits for the foreman (§3)
	KindTaskNeedsHelp          = "task_needs_help"           // the worker asked its question (§3)
	KindTaskFailed             = "task_failed"               // the run failed; the foreman decides (§3)
	KindReviewChanges          = "review_changes"            // the reviewer wants changes (§7)
	KindReviewNoVerdict        = "review_no_verdict"         // the review ended without a usable verdict (§7)
	KindTaskOutOfRounds        = "task_out_of_rounds"        // the task spent its rounds; arrives with the rounds work
	KindForemanReport          = "foreman_report"            // a done foreman turn's final message, its report to the user (§3, §10)
	KindForemanLimited         = "foreman_limited"           // the foreman's CLI hit its subscription limit (§4.4, §10)
	KindForemanWakeBudgetSpent = "foreman_wake_budget_spent" // the day's foreman wake budget is spent (§4.3, §10)
	KindAgentBroken            = "agent_broken"              // an agent was marked broken (§8 "Health", §10)
	// KindReviewUnservable: a task's queued review can never be taken —
	// no agent of another family offers review (§7, §10).
	KindReviewUnservable = "review_unservable"
	// KindPlanAwaitingApproval: a plan is over the approval limit and
	// waits for the user's yes (§6.3, §10).
	KindPlanAwaitingApproval = "plan_awaiting_approval"
)

// title is the notification's bold line: the one name every banner
// carries, whatever happened, so a glance attributes it to brigada.
const title = "brigada"

// foremanTitle is the report's bold line: the banner that speaks with
// the foreman's own voice, so a one-line report reads as the foreman's
// word and not another of the engine's alerts.
const foremanTitle = "brigada foreman"

// reportPointer is what a cut view of a foreman report ends with: the
// command that shows the whole text (§9). Mirrored by the engine's
// status view, so every short view points the same way.
func reportPointer(jobID int64) string {
	return fmt.Sprintf("… (full: brigada history --job %d)", jobID)
}

// bodyLimit bounds a notification's body line: a check's tail or a
// review's findings can be long; a banner is not the place for them.
const bodyLimit = 200

// Event is one log row in the shape the mapper needs: the delivery
// loop resolves the event's references (the task's plan, the title)
// and its payload's reason before mapping, so Message stays pure.
type Event struct {
	ID        int64
	Kind      string
	TaskID    int64 // 0 when the event names no task
	PlanID    int64 // 0 when the event names no plan
	PlanTitle string
	// JobID is the foreman job a foreman_report names (§9): the id the
	// pointer to the whole text names. 0 when the event names no job.
	JobID int64
	// Project is the event's project repo URL — the log's own project
	// reference first, else the plan its task or payload names. Empty
	// when no referent survives; the webhook carries it as-is.
	Project string
	// At is the log's created_at stamp: the time the webhook reports.
	At string
	// Reason is the payload's words for why the user is woken — the
	// check's tail, the worker's question, the review's findings —
	// already picked for the kind and flattened to one line, except a
	// report's, whose own lines the mapper cuts to the first.
	Reason string
	// Parked is true when the event parked its task (its payload names
	// needs_foreman or needs_user as the task's state). A needs-you kind
	// wakes the user only then.
	Parked bool
}

// needsForemanKinds are the event kinds whose body is "task #N needs
// you: <reason>": every way a task parks at needs_foreman (§3, §7).
// task_check_failed and review_changes also land without parking, before
// a fix round; Message skips an event that did not park (Event.Parked).
var needsForemanKinds = map[string]bool{
	KindTaskCheckFailed: true,
	KindTaskNeedsHelp:   true,
	KindTaskFailed:      true,
	KindReviewChanges:   true,
	KindReviewNoVerdict: true,
	KindTaskOutOfRounds: true,
}

// Message maps one event to its notification's title and one-line
// body (§10). ok is false for every event kind that never wakes the
// user — the log's routine entries — which the delivery loop skips
// while still advancing its cursor past them.
func Message(ev Event) (t, body string, ok bool) {
	switch {
	case ev.Kind == KindPlanDone:
		return title, oneLine(fmt.Sprintf("plan #%d done: %s", ev.PlanID, ev.PlanTitle), bodyLimit), true
	case ev.Kind == KindForemanReport:
		// A report is the foreman speaking, so its banner carries the
		// foreman's name; the body is the report's first line — written
		// for exactly this banner — never the lines after it (§3, §10).
		// A cut body — more lines followed, or the line itself ran past
		// the limit — ends with the pointer to the whole text in the
		// history (§9), the line cut to leave the pointer room. No
		// parked gate: a report is not a task waiting.
		body := firstLine(ev.Reason)
		flat := oneLine(body, bodyLimit)
		if cut := flat != body || strings.TrimSpace(ev.Reason[len(body):]) != ""; cut && ev.JobID > 0 {
			ptr := reportPointer(ev.JobID)
			body = oneLine(body, bodyLimit-len(ptr)) + ptr
		} else {
			body = flat
		}
		if body == "" {
			body = ev.Kind
		}
		return foremanTitle, body, true
	case ev.Kind == KindForemanLimited:
		// Reason is the reset time, empty when the error named none: the
		// user chooses to wait, switch the foreman, or go on without it.
		when := "reset time unknown"
		if ev.Reason != "" {
			when = "until " + ev.Reason
		}
		return title, oneLine("the foreman hit its limit ("+when+"): wait, or switch it with `brigada project foreman`", bodyLimit), true
	case ev.Kind == KindForemanWakeBudgetSpent:
		// Reason is "20/20 today, resets <time>" — the budget spent and
		// the window's end (§4.3). The user raises it, or sends a
		// message: a wake that carries one is not charged.
		when := "resets at the next UTC day"
		if ev.Reason != "" {
			when = ev.Reason
		}
		return title, oneLine("the foreman's wake budget is spent ("+when+"); raise it with `brigada budget --wakes N` or send `brigada ask`", bodyLimit), true
	case ev.Kind == KindAgentBroken:
		// Reason is "harness/model: why": the user fixes the login or
		// the runner, then resets the agent.
		return title, oneLine("agent broken, "+ev.Reason+": fix it, then `brigada agent reset <harness>/<model>`", bodyLimit), true
	case ev.Kind == KindPlanAwaitingApproval:
		// Reason is the limit's words (task count, classes, protected
		// paths); the user answers with approve or reject.
		return title, oneLine(fmt.Sprintf("plan #%d needs your approval: %s: `brigada plan approve %d` or `brigada plan reject %d`",
			ev.PlanID, ev.Reason, ev.PlanID, ev.PlanID), bodyLimit), true
	case ev.Kind == KindReviewUnservable:
		// Reason is the needs-you entry's own words: what is missing and
		// what the user can do about it.
		return title, oneLine(fmt.Sprintf("task #%d reviewing: %s", ev.TaskID, ev.Reason), bodyLimit), true
	case needsForemanKinds[ev.Kind] && ev.Parked:
		reason := ev.Reason
		if reason == "" {
			reason = ev.Kind
		}
		return title, oneLine(fmt.Sprintf("task #%d needs you: %s", ev.TaskID, reason), bodyLimit), true
	}
	return "", "", false
}

// hookText is the text the webhook posts for an event whose banner text
// is body: the same line, except that a failed check's own output never
// leaves the machine — the relay is a third party's, and a check's tail
// can carry whatever the command printed (§10). Every other kind, a
// task out of fix rounds with its reason included, posts the banner's
// line.
func hookText(ev Event, body string) string {
	if ev.Kind == KindTaskCheckFailed {
		return oneLine(fmt.Sprintf("task #%d needs you: its check failed", ev.TaskID), bodyLimit)
	}
	return body
}

// firstLine keeps s up to its first newline: the line of a report a
// banner shows, the rest left for `brigada status` and the history.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// oneLine flattens s to a single line of at most about max bytes:
// whitespace runs become one space — a reason may carry newlines — and
// the cut, when it lands inside the limit, backs off to a complete
// rune. Mirrored from the engine's status view, so a banner and the
// "needs you" queue say the same thing the same way.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 {
		_, size := utf8.DecodeLastRuneInString(s)
		if size == 1 && s[len(s)-1] >= utf8.RuneSelf {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}
