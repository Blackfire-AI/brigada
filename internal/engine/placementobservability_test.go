package engine

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/caps"
)

// The placement-observability acceptance (issue #263, criterion 9): the
// job_agent_picked event carries the whole decision — the requested and
// served tier, the family policy and resulting placement, the fallback
// taken, the retry exclusions in force, and every candidate the
// placement dropped with the reason it was dropped — and the job_cap_held
// event names the layer that made a job wait. The eight earlier criteria
// are covered by their own tests: placement_test.go (1-3, 8),
// stageretry_test.go (4-5), classcooldown_test.go (6) and
// boundedwait_test.go (7).

// TestPickRecordsExclusionAndFamilyFallback: a review retry under prefer
// whose first (cross-family) reviewer failed carries the exclusion and
// falls to a same-family reviewer — and the pick event records all of it:
// the exclusion, the dropped reviewer with its reason, the policy, the
// placement and the fallback.
func TestPickRecordsExclusionAndFamilyFallback(t *testing.T) {
	e := newEnv(t)
	author := e.agented("author", "anthropic", "implement", "review")
	first := e.agented("first", "google", "review")  // cross-family
	same := e.agented("same", "anthropic", "review") // the author's family
	e.runToReview(author)
	e.runReview(first, `{}`) // unusable: the retry excludes the first reviewer

	retry := e.int("SELECT id FROM jobs WHERE task_id = ? AND kind = 'review' ORDER BY id DESC LIMIT 1", e.task)
	if got, want := e.str("SELECT exclude_agents FROM jobs WHERE id = ?", retry), itoaList(e.agentOf(first)); got != want {
		t.Fatalf("retry exclude_agents = %s, want %s", got, want)
	}
	g := e.leaseAny(same)
	if g == nil || g.JobID != retry {
		t.Fatalf("same-family reviewer leased %+v, want the retry %d", g, retry)
	}

	evs := e.pickEvents(retry)
	if len(evs) != 1 {
		t.Fatalf("pick events = %d, want one", len(evs))
	}
	ev := evs[0]
	if ev.FamilyPolicy != ReviewFamilyPrefer {
		t.Errorf("family_policy = %q, want %q", ev.FamilyPolicy, ReviewFamilyPrefer)
	}
	if ev.Placement != placeSameFamily || ev.Fallback != placeSameFamily {
		t.Errorf("placement = %q fallback = %q, want %q for both", ev.Placement, ev.Fallback, placeSameFamily)
	}
	if !slices.Equal(ev.ExcludeAgents, []int64{e.agentOf(first)}) {
		t.Errorf("exclude_agents = %v, want the failed reviewer %d", ev.ExcludeAgents, e.agentOf(first))
	}
	if !hasRemoved(ev.Removed, e.agentOf(first), "excluded: failed the attempt") {
		t.Errorf("removed = %+v, want the failed reviewer with the exclusion reason", ev.Removed)
	}
	if ev.TierAsked == "" || ev.TierServed == "" {
		t.Errorf("tier = %q -> %q, want both named", ev.TierAsked, ev.TierServed)
	}
}

// TestPickRecordsTierClimb: a standard task served only by heavy agents
// climbs to heavy, and the pick records the requested tier and the
// served one.
func TestPickRecordsTierClimb(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Tier = map[string]string{"claude/opus": "heavy"}
	})
	e.addAgent(e.runner1, "claude", "opus", "work")
	job := e.tierWork("standard")
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != job.JobID {
		t.Fatalf("lease = %+v, want job %d", g, job.JobID)
	}
	evs := e.pickEvents(job.JobID)
	if len(evs) != 1 || evs[0].TierAsked != "standard" || evs[0].TierServed != "heavy" {
		t.Fatalf("pick = %+v, want standard asked served heavy", evs)
	}
}

// TestPickRecordsDroppedCandidates: agents that do not offer the class,
// sit below the floor, or are cooling are dropped and named with the
// reason, while the one that can take the job wins.
func TestPickRecordsDroppedCandidates(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) { c.RunnerStale = time.Hour })
	noClass := e.addAgent(e.runner1, "claude", "haiku", "work")
	e.exec("UPDATE agents SET classes = '[\"review\"]' WHERE id = ?", noClass)
	cooling := e.addAgent(e.runner1, "codex", "gpt-5", "work")
	e.coolUntil(cooling, time.Hour)
	winner := e.addAgent(e.runner1, "claude", "sonnet", "work")

	job := e.tierWork("standard")
	g := e.leaseAny(e.runner1)
	if g == nil || g.JobID != job.JobID || e.jobAgent(job.JobID) != winner {
		t.Fatalf("lease = %+v, want job %d on %d", g, job.JobID, winner)
	}
	evs := e.pickEvents(job.JobID)
	if len(evs) != 1 {
		t.Fatalf("pick events = %d, want one", len(evs))
	}
	for agent, reason := range map[int64]string{
		noClass: "class implement not offered",
		cooling: "cooling until",
	} {
		if !hasRemoved(evs[0].Removed, agent, reason) {
			t.Errorf("removed = %+v, want agent %d with %q", evs[0].Removed, agent, reason)
		}
	}
}

// TestWaitRecordsTheLayer: a job held by a full cap layer names the
// layer in its job_cap_held event, so the wait is explained.
func TestWaitRecordsTheLayer(t *testing.T) {
	e := newEnvCfg(t, func(c *Config) {
		c.Caps.Account = map[string]int{"work": 1}
		c.LeaseTTL = time.Hour
	})
	e.addAgent(e.runner1, "codex", "gpt-5", "work")
	e.submit(e.work())
	if g := e.leaseAny(e.runner1); g == nil {
		t.Fatal("nothing to lease")
	}
	held := e.submit(e.workOn(e.addTask("{}")))
	if g := e.leaseAny(e.runner1); g != nil {
		t.Fatalf("the cap-full job leaked out: %+v", g)
	}
	if layers := e.capHeldLayers(held.JobID); len(layers) != 1 || layers[0] != caps.LayerAccount {
		t.Fatalf("job_cap_held layers = %v, want [account]", layers)
	}
}

// hasRemoved reports whether removed names agent with a reason that
// contains want.
func hasRemoved(removed []Removed, agent int64, want string) bool {
	for _, r := range removed {
		if r.Agent == agent && strings.Contains(r.Reason, want) {
			return true
		}
	}
	return false
}

// itoaList renders one agent id as the stored exclude_agents JSON list.
func itoaList(id int64) string {
	return "[" + strconv.FormatInt(id, 10) + "]"
}
