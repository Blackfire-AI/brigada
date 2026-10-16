package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/notify"
)

// TestReviewUnservable: a queued review is unservable only when no
// registered agent could ever take it — none of another family than the
// author's (and not the first reviewer's agent, on a retry) offers the
// review class. Cooling or broken agents of the right family make it
// merely wait: those are temporary, a missing reviewer is not.
func TestReviewUnservable(t *testing.T) {
	type agent struct {
		family  string
		classes []string
		health  string // "" = ready
		cooling bool
	}
	review := []string{"implement", "review"}
	tests := []struct {
		name          string
		agents        []agent
		excludeFamily string
		excludeAgent  int // 1-based index into agents; 0 = none
		kind          Kind
		want          bool
	}{
		{"one family only", []agent{{family: "anthropic", classes: review}}, "anthropic", 0, KindReview, true},
		{"other family ready", []agent{{family: "anthropic", classes: review}, {family: "google", classes: review}}, "anthropic", 0, KindReview, false},
		{"other family cooling", []agent{{family: "anthropic", classes: review}, {family: "google", classes: review, cooling: true}}, "anthropic", 0, KindReview, false},
		{"other family broken", []agent{{family: "anthropic", classes: review}, {family: "google", classes: review, health: "broken"}}, "anthropic", 0, KindReview, false},
		{"other family offers no review class", []agent{{family: "anthropic", classes: review}, {family: "google", classes: []string{"implement"}}}, "anthropic", 0, KindReview, true},
		{"retry: the only other-family agent is the first reviewer", []agent{{family: "anthropic", classes: review}, {family: "google", classes: review}}, "anthropic", 2, KindReview, true},
		{"retry: a second other-family agent remains", []agent{{family: "anthropic", classes: review}, {family: "google", classes: review}, {family: "openai", classes: review}}, "anthropic", 2, KindReview, false},
		{"no agents at all", nil, "anthropic", 0, KindReview, true},
		{"author without a family excludes nothing", []agent{{family: "anthropic", classes: []string{"implement"}}}, "", 0, KindReview, false},
		{"not a review job", []agent{{family: "anthropic", classes: review}}, "anthropic", 0, KindWork, false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			var ids []int64
			for j, a := range tt.agents {
				runner := e.agented(fmt.Sprintf("h%d-%d", i, j), a.family, a.classes...)
				id := e.agentOf(runner)
				ids = append(ids, id)
				if a.health != "" {
					e.exec("UPDATE agents SET health = ? WHERE id = ?", a.health, id)
				}
				if a.cooling {
					e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(e.clock.Now().Add(time.Hour)), id)
				}
			}
			j := &jobRow{Kind: tt.kind, ExcludeFamily: tt.excludeFamily}
			if tt.excludeAgent != 0 {
				j.ExcludeAgent = ids[tt.excludeAgent-1]
			}
			var got bool
			err := e.st.View(e.ctx, func(tx *sql.Tx) error {
				var err error
				got, err = reviewUnservableTx(tx, j)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("reviewUnservableTx = %v, want %v", got, tt.want)
			}
		})
	}
}

const wantNoReviewer = "no reviewer: every agent offering review is of the author's family (anthropic); " +
	"offer an agent of another family to the runner (--agent harness=…) or review by hand"

// needsYouTask returns the task's needs-you entry, if any.
func (e *env) needsYouTask(task int64) (NeedsYouItem, bool) {
	e.t.Helper()
	info, err := e.eng.Status(e.ctx, 0)
	if err != nil {
		e.t.Fatalf("status: %v", err)
	}
	for _, it := range info.NeedsYou {
		if it.Kind == "task" && it.ID == task {
			return it, true
		}
	}
	return NeedsYouItem{}, false
}

// TestStatusListsAnUnservableReview: while a task's review waits with no
// reviewer of another family, status says so; the entry goes when one
// registers.
func TestStatusListsAnUnservableReview(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement", "review")
	e.runToReview(author)

	it, ok := e.needsYouTask(e.task)
	if !ok {
		t.Fatal("no needs-you entry for the task with no reviewer")
	}
	if it.State != taskReviewing || it.Reason != wantNoReviewer {
		t.Errorf("entry = %+v, want state reviewing and reason %q", it, wantNoReviewer)
	}

	e.agented("reviewer", "google", "review")
	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("entry %+v remains after an agent of another family registered", it)
	}
}

// TestCoolingReviewerIsNotMissing: a cooling agent of another family is
// a wait, not a structural gap: no entry, no notification.
func TestCoolingReviewerIsNotMissing(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement", "review")
	reviewer := e.agented("reviewer", "google", "review")
	e.runToReview(author)
	e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(e.clock.Now().Add(time.Hour)), e.agentOf(reviewer))

	if it, ok := e.needsYouTask(e.task); ok {
		t.Errorf("entry %+v for a review whose reviewer is only cooling", it)
	}
	if n := e.int("SELECT COUNT(*) FROM events WHERE kind = ?", evReviewUnservable); n != 0 {
		t.Errorf("review_unservable events = %d, want none", n)
	}
}

// TestQueueTTLParkReasonNamesTheMissingReviewer: the TTL park says why
// when the review was unservable, and keeps the bare wording otherwise.
func TestQueueTTLParkReasonNamesTheMissingReviewer(t *testing.T) {
	for _, tt := range []struct {
		name         string
		otherFamily  bool
		want, wantNo string
	}{
		{"unservable", false, "no reviewer: no agent of a family other than anthropic offers review", "queue TTL exceeded"},
		{"merely waiting", true, "queue TTL exceeded: no runner leased the job within", "no reviewer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnvCfg(t, func(c *Config) { c.QueueTTL = 10 * time.Minute })
			author := e.agented("author", "anthropic", "implement", "review")
			review := e.runToReview(author)
			if tt.otherFamily {
				r := e.agented("reviewer", "google", "review")
				e.exec("UPDATE agents SET cooling_until = ? WHERE id = ?", fmtTime(e.clock.Now().Add(time.Hour)), e.agentOf(r))
			}
			e.clock.Advance(11 * time.Minute)
			rep := e.mustSweep()
			if len(rep.QueueTimedOut) != 1 || rep.QueueTimedOut[0] != review {
				t.Fatalf("queue timeouts = %v, want the review job %d", rep.QueueTimedOut, review)
			}
			p, ok := e.eventPayloadOf(evTaskFailed)
			if !ok {
				t.Fatal("no park event")
			}
			if !strings.Contains(p.Reason, tt.want) || strings.Contains(p.Reason, tt.wantNo) {
				t.Errorf("park reason = %q, want %q and not %q", p.Reason, tt.want, tt.wantNo)
			}
			if res := e.str("SELECT result FROM jobs WHERE id = ?", review); !strings.Contains(res, tt.want) {
				t.Errorf("job result = %q, want %q", res, tt.want)
			}
		})
	}
}

// TestUnservableReviewNotifiesOnce: the first detection raises one
// notification through the delivery loop; repeated status calls and
// sweeps raise no more.
func TestUnservableReviewNotifiesOnce(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement", "review")
	e.runToReview(author)

	fake := &notify.Fake{}
	loop := notify.NewLoop(e.st, fake, nil)
	for i := 0; i < 3; i++ {
		e.needsYouTask(e.task)
		e.mustSweep()
		if err := loop.Flush(e.ctx); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	var bodies []string
	for _, n := range fake.Notes() {
		if strings.Contains(n.Body, "no reviewer") {
			bodies = append(bodies, n.Body)
		}
	}
	want := fmt.Sprintf("task #%d reviewing: %s", e.task, wantNoReviewer)
	if len(bodies) != 1 || bodies[0] != want {
		t.Errorf("notifications = %q, want exactly %q", bodies, want)
	}
}
