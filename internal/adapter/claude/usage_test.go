package claude

import (
	"bufio"
	"github.com/Blackfire-AI/brigada/internal/adapter"
	"os"
	"reflect"
	"testing"
)

func usageInt(n int64) *int64       { return &n }
func usageFloat(n float64) *float64 { return &n }
func TestUsageFixture(t *testing.T) {
	f, err := os.Open("testdata/usage.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	wants := []*adapter.Usage{&adapter.Usage{InputTokens: usageInt(15), OutputTokens: usageInt(5), CachedInputTokens: usageInt(2), CostUSD: usageFloat(0.25), ModelCalls: usageInt(4), CostCumulative: true},
		&adapter.Usage{InputTokens: usageInt(0), CostUSD: usageFloat(0), CostCumulative: true},
		nil,
		&adapter.Usage{OutputTokens: usageInt(7)}}
	scanner := bufio.NewScanner(f)
	i := 0
	for scanner.Scan() {
		ev, ok := (&Adapter{}).ParseEvent(scanner.Bytes())
		if i >= len(wants) {
			t.Fatal("too many lines")
		}
		if !reflect.DeepEqual(ev.Usage, wants[i]) {
			t.Errorf("line %d usage = %+v, want %+v", i, ev.Usage, wants[i])
		}
		if wants[i] != nil && !ok {
			t.Errorf("line %d usage event discarded", i)
		}
		if ev.Kind == adapter.EvUsage && ev.IsProgress() {
			t.Fatal("usage advances progress")
		}
		i++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if i != len(wants) {
		t.Fatalf("read %d lines, want %d", i, len(wants))
	}
}

// A resumed session's result carries the session's running total: the
// adapter reports it flagged cumulative and leaves tokens and turns as the
// invocation's own.
func TestResumedResultCostIsCumulative(t *testing.T) {
	line := `{"type":"result","result":"ok","session_id":"s1","usage":{"input_tokens":10,"output_tokens":5},"num_turns":2,"total_cost_usd":3.5}`
	ev, ok := (&Adapter{}).ParseEvent([]byte(line))
	if !ok || ev.Usage == nil {
		t.Fatal("no usage")
	}
	want := &adapter.Usage{InputTokens: usageInt(10), OutputTokens: usageInt(5), CostUSD: usageFloat(3.5), ModelCalls: usageInt(2), CostCumulative: true}
	if !reflect.DeepEqual(ev.Usage, want) {
		t.Fatalf("usage = %+v, want %+v", ev.Usage, want)
	}
}
