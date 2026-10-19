package kimi

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
	wants := []*adapter.Usage{&adapter.Usage{InputTokens: usageInt(10), OutputTokens: usageInt(5), CachedInputTokens: usageInt(2), ReasoningTokens: usageInt(3)},
		nil,
		nil}
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
