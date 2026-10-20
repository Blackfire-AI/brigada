package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter/codex"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

type usageTransport func(*http.Request) (*http.Response, error)

func (f usageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestJobUsageMultiLineCompletion(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var got struct {
		WallSeconds *float64 `json:"wall_seconds"`
	}
	var payload map[string]json.RawMessage
	client := api.NewClientWithHTTPClient(&http.Client{Transport: usageTransport(func(req *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"answer":"ok"}`)), Header: make(http.Header)}, nil
	})}, "test-token")
	r := &jobRun{cfg: &Config{Clock: Clock{Now: func() time.Time { return now }}}, client: client, grant: &api.JobGrant{JobID: 1, LeaseID: "lease"}, startedAt: now.Add(-3 * time.Second)}
	ad := &codex.Adapter{}
	for _, line := range []string{
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5,"cached_input_tokens":2,"reasoning_output_tokens":1}}`,
		`{"type":"turn.completed","usage":"malformed"}`,
		`{"type":"turn.completed","usage":{"input_tokens":20,"output_tokens":7,"cached_input_tokens":0,"reasoning_output_tokens":3}}`,
	} {
		r.recordEvent(ad, []byte(line), false)
	}
	if !r.lastProgress.IsZero() {
		t.Fatal("usage advanced progress")
	}
	if ans, err := r.complete(context.Background(), api.Completion{Result: "done"}); err != nil || ans != engine.AnsOK {
		t.Fatalf("complete: %s %v", ans, err)
	}
	var usage map[string]int64
	if err := json.Unmarshal(payload["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]int64{"input_tokens": 30, "output_tokens": 12, "cached_input_tokens": 2, "reasoning_tokens": 4, "model_calls": 2} {
		if usage[field] != want {
			t.Errorf("%s = %d, want %d", field, usage[field], want)
		}
	}
	if _, ok := usage["cost_usd"]; ok {
		t.Fatal("invented dollar cost")
	}
	if got.WallSeconds == nil || *got.WallSeconds != 3 {
		t.Fatalf("wall time = %v", got.WallSeconds)
	}
	// A separate resumed job uses only the counters it reports.
	second := &jobRun{cfg: r.cfg, client: client, grant: r.grant, startedAt: now}
	second.recordEvent(ad, []byte(`{"type":"turn.completed","usage":{"input_tokens":1}}`), false)
	if ans, err := second.complete(context.Background(), api.Completion{Result: "done"}); err != nil || ans != engine.AnsOK {
		t.Fatal(ans, err)
	}
	usage = nil
	if err := json.Unmarshal(payload["usage"], &usage); err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 || usage["input_tokens"] != 1 || usage["model_calls"] != 1 {
		t.Fatalf("usage carried across jobs: %s", payload["usage"])
	}
	if sumUsage(nil) != nil {
		t.Fatal("unreported usage became known")
	}
}
