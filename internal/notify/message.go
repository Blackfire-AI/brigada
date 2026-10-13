package notify

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// The event kinds the engine's log can carry that wake the user (§10).
// The set is the notification vocabulary: a plan finishing, and the
// reasons a task parks at needs_foreman — the same kinds the "needs
// you" queue reads in `brigada status`. Kinds the engine adds later
// (the foreman's limit and budget-out, §4.4, §4.3) slot into the table
// below; everything else in the log never notifies.
const (
	KindPlanDone        = "plan_done"          // the plan's last open task finished (§6.2)
	KindTaskCheckFailed = "task_check_failed"  // the check failed; the task waits for the foreman (§3)
	KindTaskNeedsHelp   = "task_needs_help"    // the worker asked its question (§3)
	KindTaskFailed      = "task_failed"        // the run failed; the foreman decides (§3)
	KindReviewChanges   = "review_changes"     // the reviewer wants changes (§7)
	KindReviewNoVerdict = "review_no_verdict"  // the review ended without a usable verdict (§7)
	KindTaskOutOfRounds = "task_out_of_rounds" // the task spent its rounds; arrives with the rounds work
)

// title is the notification's bold line: the one name every banner
// carries, whatever happened, so a glance attributes it to brigada.
const title = "brigada"

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
	// Reason is the payload's words for why the user is woken — the
	// check's tail, the worker's question, the review's findings —
	// already picked for the kind and flattened to one line.
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
	case needsForemanKinds[ev.Kind] && ev.Parked:
		reason := ev.Reason
		if reason == "" {
			reason = ev.Kind
		}
		return title, oneLine(fmt.Sprintf("task #%d needs you: %s", ev.TaskID, reason), bodyLimit), true
	}
	return "", "", false
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
