package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

type statsUsageTransport func(*http.Request) (*http.Response, error)

func (f statsUsageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStatsToolUsage(t *testing.T) {
	const response = `[{"project_id":1,"repo_url":"example.invalid/repo","rows":[{"agent_id":2,"class":"implement","input_tokens":10,"output_tokens":5,"cached_input_tokens":0,"reasoning_tokens":null,"tokens":15,"cost_usd":0,"wall_seconds":2.5,"cost_per_success":0,"cost_per_success_unit":"USD"}]}]`
	client := api.NewClientWithHTTPClient(&http.Client{Transport: statsUsageTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/stats" {
			t.Fatalf("request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})}, "test-token")
	text, err := callStats(context.Background(), client, nil)
	if err != nil {
		t.Fatal(err)
	}
	var projects []engine.ProjectStats
	if err := json.Unmarshal([]byte(text), &projects); err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || len(projects[0].Rows) != 1 {
		t.Fatal(text)
	}
	row := projects[0].Rows[0]
	if row.InputTokens == nil || *row.InputTokens != 10 || row.OutputTokens == nil || *row.OutputTokens != 5 || row.CachedInputTokens == nil || *row.CachedInputTokens != 0 || row.ReasoningTokens != nil || row.Tokens == nil || *row.Tokens != 15 || row.CostUSD == nil || *row.CostUSD != 0 || row.WallSeconds == nil || *row.WallSeconds != 2.5 || row.CostPerSuccess == nil || *row.CostPerSuccess != 0 || row.CostPerSuccessUnit != "USD" {
		t.Fatal(text)
	}
}
