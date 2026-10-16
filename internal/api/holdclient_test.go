package api

import (
	"context"
	"encoding/json"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type holdTransport func(*http.Request) (*http.Response, error)

func (f holdTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHoldClientFenceAndSingleAttempt(t *testing.T) {
	calls := 0
	c := &Client{token: "hold-secret", fence: 17, http: &http.Client{Transport: holdTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer hold-secret" || r.Header.Get(holdFenceHeader) != "17" {
			t.Errorf("missing hold authority: %v", r.Header)
		}
		return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(`{"code":"stale_hold","error":"acquire explicitly"}`)), Header: make(http.Header), Request: r}, nil
	})}}
	_, err := c.DecisionNote(context.Background(), "old decision", "idem")
	se, ok := err.(*StatusError)
	if !ok || se.APICode != "stale_hold" || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestProjectReaderScopesReads(t *testing.T) {
	transport := holdTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("project") != "42" {
			t.Errorf("unscoped read: %s", r.URL)
		}
		body := "{}"
		if r.URL.Path == "/v1/stats" {
			body = "[]"
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	bootstrap := &Client{token: "client-secret", http: &http.Client{Transport: transport}}
	c := bootstrap.ProjectReader(42)
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.History(context.Background(), HistoryQuery{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stats(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Capacity(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if bootstrap.readProject != 0 {
		t.Fatal("mutated bootstrap scope")
	}
}

func TestAcquireHoldMetadataAndCredentialIsolation(t *testing.T) {
	c := &Client{token: "client-secret", http: &http.Client{Transport: holdTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get(holdFenceHeader) != "" || r.Header.Get("Authorization") != "Bearer client-secret" {
			t.Error("acquire used hold authority")
		}
		var body HoldAcquireRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.BridgeInstance != "bridge" || body.HarnessSessionID != "session" || body.HarnessSessionSource != "env" {
			t.Fatalf("body=%+v", body)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"credential":"minted-secret","fence":12,"hold_id":9}`)), Header: make(http.Header), Request: r}, nil
	})}}
	res, err := c.AcquireHold(context.Background(), HoldAcquireRequest{RepoURL: "example.com/repo", BridgeInstance: "bridge", HarnessSessionID: "session", HarnessSessionSource: "env"})
	if err != nil {
		t.Fatal(err)
	}
	held := c.HoldClient(res.Credential, res.Fence)
	if held.token != "minted-secret" || held.fence != 12 || c.token != "client-secret" || c.fence != 0 {
		t.Fatal("credential contexts mixed")
	}
}

func TestInteractiveAPIWithoutNetwork(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tokens := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokens, []byte("c client local-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := engine.New(st, engine.Config{})
	for _, repo := range []string{"https://example.invalid/one.git", "https://example.invalid/two.git"} {
		if _, err := eng.SetProjectForeman(ctx, repo, "claude", "sonnet"); err != nil {
			t.Fatal(err)
		}
	}
	server, err := New(ctx, st, eng, nil, tokens)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	transport := holdTransport(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Result(), nil
	})
	bootstrap := &Client{token: "local-secret", http: &http.Client{Transport: transport}}
	res, err := bootstrap.AcquireHold(ctx, HoldAcquireRequest{RepoURL: "https://example.invalid/one.git", BridgeInstance: "bridge"})
	if err != nil {
		t.Fatal(err)
	}
	held := bootstrap.HoldClient(res.Credential, res.Fence)
	question, err := held.AskUser(ctx, "Choose a default?", "ask-once")
	if err != nil || question == 0 {
		t.Fatalf("question=%d err=%v", question, err)
	}
	again, err := held.AskUser(ctx, "Choose a default?", "ask-once")
	if err != nil || again != question {
		t.Fatalf("duplicate question=%d err=%v", again, err)
	}
	if _, err := bootstrap.AskUser(ctx, "unauthorized", "bad"); err == nil {
		t.Fatal("client called foreman mutation")
	}
	if _, err := held.ProjectReader(2).Status(ctx); err == nil {
		t.Fatal("foreman widened project scope")
	}
	if _, err := held.HeartbeatHold(ctx); err != nil {
		t.Fatal(err)
	}
	if err := held.YieldHold(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := held.DecisionNote(ctx, "stale", "must-not-apply"); err == nil {
		t.Fatal("stale mutation applied")
	}
	if _, err := held.HeartbeatHold(ctx); err == nil {
		t.Fatal("stale renewal accepted")
	}
	if err := held.YieldHold(ctx); err == nil {
		t.Fatal("stale yield accepted")
	}
}
