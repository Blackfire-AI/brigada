package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

type stubStats struct {
	stats []engine.ProjectStats
	err   error
}

func (s stubStats) Stats(context.Context, int64) ([]engine.ProjectStats, error) {
	return s.stats, s.err
}

// TestStatsTable renders the stats from a stubbed client: aligned
// columns per repo, the confidence words, the unrecorded agent at
// attempts 0 and the stale runner marked.
func TestStatsTable(t *testing.T) {
	stub := stubStats{stats: []engine.ProjectStats{
		{ProjectID: 1, RepoURL: "https://example.invalid/one.git", Rows: []engine.StatsRow{
			{Harness: "claude", Model: "sonnet", Tier: "heavy", Live: true, Class: "implement", Attempts: 6, Successes: 5, Score: engine.Score(6, 5)},
			{Harness: "kimi", Model: "k2", Tier: "standard", Live: true, Class: "implement", Attempts: 2, Successes: 1, Score: engine.Score(2, 1)},
			{Harness: "zcode", Model: "glm", Tier: "light", Live: false, Class: "implement", Score: engine.Score(0, 0)},
		}},
		{ProjectID: 2, RepoURL: "https://example.invalid/two.git", Rows: []engine.StatsRow{}},
	}}
	var out, errb bytes.Buffer
	if code := printStats(context.Background(), stub, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	want := "https://example.invalid/one.git\n" +
		"  AGENT                TIER      CLASS      ATTEMPTS  SUCCESSES  RATE  CONFIDENCE  INPUT OUTPUT CACHED REASONING TOKENS CALLS CALLS/JOB CALLS/SUCCESS USD WALL(s) COST/SUCCESS\n" +
		"  claude/sonnet        heavy     implement  6         5          0.79  enough - - - - - - - - - - -\n" +
		"  kimi/k2              standard  implement  2         1          0.50  starting - - - - - - - - - - -\n" +
		"  zcode/glm (stale)    light     implement  0         0          0.50  none - - - - - - - - - - -\n" +
		"\n" +
		"https://example.invalid/two.git\n" +
		"  no agents registered\n"
	// Compare field by field so the column padding is not the test's
	// concern, only the content and the order.
	gotLines := strings.Split(out.String(), "\n")
	wantLines := strings.Split(want, "\n")
	if len(gotLines) != len(wantLines) {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
	for i := range wantLines {
		if strings.Join(strings.Fields(gotLines[i]), " ") != strings.Join(strings.Fields(wantLines[i]), " ") {
			t.Errorf("line %d = %q, want %q", i, gotLines[i], wantLines[i])
		}
	}
	// Aligned: every table row puts CLASS's column at the same offset.
	col := strings.Index(gotLines[1], "CLASS")
	for _, l := range gotLines[2:5] {
		if !strings.HasPrefix(l[col:], "implement") {
			t.Errorf("row %q: class column is not aligned under CLASS", l)
		}
	}
}

func TestStatsClientError(t *testing.T) {
	var out, errb bytes.Buffer
	if code := printStats(context.Background(), stubStats{err: errors.New("boom")}, &out, &errb); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if out.Len() != 0 || !strings.Contains(errb.String(), "boom") {
		t.Errorf("stdout %q stderr %q, want only the error", out.String(), errb.String())
	}
}

func TestStatsConfidence(t *testing.T) {
	for attempts, want := range map[int]string{0: "none", 1: "starting", 4: "starting", 5: "enough", 20: "enough"} {
		if got := statsConfidence(attempts); got != want {
			t.Errorf("statsConfidence(%d) = %q, want %q", attempts, got, want)
		}
	}
}

// TestStatsListedInUsage keeps the command in the usage text and the
// command map.
func TestStatsListedInUsage(t *testing.T) {
	if !strings.Contains(usage, "\tstats ") {
		t.Errorf("usage does not list stats:\n%s", usage)
	}
	if _, ok := commands["stats"]; !ok {
		t.Errorf("stats is not in the command map")
	}
}

func TestStatsUsageColumns(t *testing.T) {
	input, output, cached, reasoning, tokens := int64(30), int64(12), int64(0), int64(4), int64(42)
	dollars, wall, cost, tokenCost := 0.25, 5.0, 0.125, 21.0
	calls, perJob, perSuccess := int64(9), 4.5, 4.5
	var out bytes.Buffer
	rows := []engine.StatsRow{
		{Harness: "claude", Model: "sonnet", Class: "implement", Live: true, Successes: 2, InputTokens: &input, OutputTokens: &output, CachedInputTokens: &cached, ReasoningTokens: &reasoning, Tokens: &tokens, ModelCalls: &calls, CallsPerJob: &perJob, CallsPerSuccess: &perSuccess, CostUSD: &dollars, WallSeconds: &wall, CostPerSuccess: &cost, CostPerSuccessUnit: "USD"},
		{Harness: "codex", Class: "review", Live: true, Tokens: &tokens, CostPerSuccess: &tokenCost, CostPerSuccessUnit: "tokens"},
	}
	if err := renderStats(&out, []engine.ProjectStats{{RepoURL: "example.invalid/repo", Rows: rows}}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.HasSuffix(strings.Join(strings.Fields(lines[2]), " "), "30 12 0 4 42 9 4.5 4.5 0.2500 5.0000 0.1250 USD") {
		t.Fatalf("dollar row: %s", lines[2])
	}
	if !strings.HasSuffix(strings.Join(strings.Fields(lines[3]), " "), "- - - - 42 - - - - - 21.0000 tokens") {
		t.Fatalf("token row: %s", lines[3])
	}
}
