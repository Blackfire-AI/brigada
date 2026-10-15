package engine

import (
	"math"
	"testing"
)

// TestScoreMath covers the score (§8): a prior of one pseudo-trial at
// 50%, so no record reads 0.5 and every attempt pulls toward the
// observed rate without reaching it.
func TestScoreMath(t *testing.T) {
	tests := []struct {
		attempts, successes int
		want                float64
	}{
		{0, 0, 0.5},
		{1, 1, 0.75},
		{1, 0, 0.25},
		{4, 4, 4.5 / 5},
		{10, 0, 0.5 / 11},
		{10, 10, 10.5 / 11},
	}
	for _, tc := range tests {
		if got := Score(tc.attempts, tc.successes); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("Score(%d, %d) = %v, want %v", tc.attempts, tc.successes, got, tc.want)
		}
	}
	// The prior's pull: a perfect short record stays under a strong
	// longer one.
	if Score(1, 1) >= Score(20, 19) {
		t.Errorf("1/1 scores %v, at or over 19/20's %v: the prior should pull the short record harder", Score(1, 1), Score(20, 19))
	}
}

// TestStatsJoinAndColdStart covers Stats (§8): an outcome row carries
// its agent's harness, model, account and runner, an offered class with
// no record is listed at attempts 0 and the prior score, the best
// score leads its class, and liveness follows the runner's last_seen_at.
func TestStatsJoinAndColdStart(t *testing.T) {
	e := newEnv(t)
	claude := e.addAgent(e.runner1, "claude", "sonnet", "work")
	kimi := e.addAgent(e.runner2, "kimi", "k2", "home")
	// runner-1 was seen at the clock's moment; runner-2 long before.
	e.exec(`UPDATE runners SET last_seen_at = ? WHERE id = ?`, seedTS, e.runner1)
	e.exec(`UPDATE runners SET last_seen_at = ? WHERE id = ?`,
		e.clock.Now().Add(-2*statsLiveWindow).UTC().Format(TimeLayout), e.runner2)
	e.exec(`INSERT INTO outcomes (project_id, agent_id, class, attempts, successes, updated_at)
		VALUES (?, ?, 'implement', 4, 3, ?)`, e.project, claude, seedTS)

	stats, err := e.eng.Stats(e.ctx, 0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(stats) != 1 || stats[0].ProjectID != e.project || stats[0].RepoURL != "https://example.invalid/brigada.git" {
		t.Fatalf("stats = %+v, want the one project", stats)
	}
	rows := stats[0].Rows
	// Two agents x three classes (implement, review, answer).
	if len(rows) != 6 {
		t.Fatalf("rows = %d, want 6: %+v", len(rows), rows)
	}
	find := func(agent int64, class string) StatsRow {
		t.Helper()
		for _, r := range rows {
			if r.AgentID == agent && r.Class == class {
				return r
			}
		}
		t.Fatalf("no row for agent %d class %s", agent, class)
		return StatsRow{}
	}
	got := find(claude, "implement")
	if got.Harness != "claude" || got.Model != "sonnet" || got.Account != "work" || got.Runner != "runner-1" {
		t.Errorf("joined row = %+v, want claude/sonnet on work via runner-1", got)
	}
	if got.Attempts != 4 || got.Successes != 3 || math.Abs(got.Score-3.5/5) > 1e-12 {
		t.Errorf("record = %d/%d score %v, want 3/4 scoring 0.7", got.Successes, got.Attempts, got.Score)
	}
	if !got.Live {
		t.Errorf("runner-1 seen at the clock's moment: Live = false, want true")
	}
	cold := find(kimi, "implement")
	if cold.Attempts != 0 || cold.Successes != 0 || cold.Score != 0.5 {
		t.Errorf("unrecorded agent = %+v, want attempts 0 at score 0.5", cold)
	}
	if cold.Harness != "kimi" || cold.Model != "k2" || cold.Live {
		t.Errorf("unrecorded agent = %+v, want kimi/k2 on a stale runner", cold)
	}
	if r := find(claude, "review"); r.Attempts != 0 || r.Score != 0.5 {
		t.Errorf("claude review = %+v, want a cold start", r)
	}
	// Within the implement class the recorded 0.7 leads the 0.5 prior.
	var implement []StatsRow
	for _, r := range rows {
		if r.Class == "implement" {
			implement = append(implement, r)
		}
	}
	if len(implement) != 2 || implement[0].AgentID != claude {
		t.Errorf("implement rows = %+v, want the better score (claude) first", implement)
	}
}

// TestStatsScope covers the project filter and a record on a class the
// agent no longer offers: the outcome still shows, once.
func TestStatsScope(t *testing.T) {
	e := newEnv(t)
	agent := e.addAgent(e.runner1, "claude", "sonnet", "work")
	e.exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES ('https://example.invalid/other.git', 'main', '{}', 'claude', 'sonnet', ?, ?)`, seedTS, seedTS)
	e.exec(`INSERT INTO outcomes (project_id, agent_id, class, attempts, successes, updated_at)
		VALUES (?, ?, 'legacy', 2, 1, ?)`, e.project, agent, seedTS)

	all, err := e.eng.Stats(e.ctx, 0)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all projects = %d, want 2", len(all))
	}
	one, err := e.eng.Stats(e.ctx, e.project)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(one) != 1 || one[0].ProjectID != e.project {
		t.Fatalf("scoped stats = %+v, want just project %d", one, e.project)
	}
	legacy := 0
	for _, r := range one[0].Rows {
		if r.Class == "legacy" {
			legacy++
			if r.Attempts != 2 || r.Successes != 1 {
				t.Errorf("legacy row = %+v, want 1 of 2", r)
			}
		}
	}
	if legacy != 1 {
		t.Errorf("legacy rows = %d, want 1", legacy)
	}
}
