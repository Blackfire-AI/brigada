package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/adapter"
)

func TestCompletionUsageProtocol(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "older", true: "reported"}[known], func(t *testing.T) {
			zero := int64(0)
			wall := 2.5
			comp := Completion{Result: "done"}
			if known {
				comp.Usage = &adapter.Usage{InputTokens: &zero}
				comp.WallSeconds = &wall
			}
			client := NewClientWithHTTPClient(&http.Client{Transport: holdTransport(func(req *http.Request) (*http.Response, error) {
				data, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				var got completeRequest
				if err := json.Unmarshal(data, &got); err != nil {
					t.Fatal(err)
				}
				if got.JobID != 7 || got.LeaseID != "lease" || got.Result != "done" {
					t.Fatalf("completion: %+v", got)
				}
				if known {
					if got.Usage == nil || got.Usage.InputTokens == nil || *got.Usage.InputTokens != 0 || got.Usage.OutputTokens != nil || got.WallSeconds == nil || *got.WallSeconds != wall {
						t.Fatalf("usage changed: %s", data)
					}
				} else if got.Usage != nil || got.WallSeconds != nil || strings.Contains(string(data), `"usage"`) {
					t.Fatalf("unknown usage became known: %s", data)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"answer":"ok"}`))}, nil
			})}, "test-token")
			if ans, err := client.Complete(context.Background(), 7, "lease", comp); err != nil || ans != "ok" {
				t.Fatal(ans, err)
			}
		})
	}
}
