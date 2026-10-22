package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestValidWebhookURL: https anywhere, http on a loopback host only —
// the URL may carry a token, which plain http would put on the wire.
func TestValidWebhookURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://relay.example.com/hook", true},
		{"http://127.0.0.1:8080/hook", true},
		{"http://localhost/hook", true},
		{"http://[::1]:9/hook", true},
		{"http://relay.example.com/hook", false},
		{"http://hook.example.test/hook", false},
		{"ftp://relay.example.com/hook", false},
		{"https:///hook", false},
		{"relay.example.com/hook", false},
	} {
		if err := ValidWebhookURL(tc.url); (err == nil) != tc.ok {
			t.Errorf("ValidWebhookURL(%q) = %v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}

// TestWebhookRefusesPlainHTTPAwayFromLoopback: a Webhook built around a
// non-loopback http URL posts nothing, whatever the config reader let by.
func TestWebhookRefusesPlainHTTPAwayFromLoopback(t *testing.T) {
	var posted atomic.Int32
	hook := &Webhook{URL: "http://relay.example.com/hook", Client: &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			posted.Add(1)
			return nil, http.ErrNotSupported
		}),
	}}
	err := hook.Post(context.Background(), Event{Kind: KindPlanDone}, "x")
	if err == nil {
		t.Fatal("post to plain http off loopback succeeded, want a refusal")
	}
	if strings.Contains(err.Error(), "relay.example.com") {
		t.Errorf("error %q names the host", err)
	}
	if posted.Load() != 0 {
		t.Errorf("%d requests went out, want none", posted.Load())
	}
}

// TestWebhookDoesNotFollowRedirects: a relay that redirects is a failed
// delivery, and the target it names never sees the post.
func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	err := (&Webhook{URL: redirector.URL + "/hook"}).Post(context.Background(), Event{Kind: KindPlanDone}, "x")
	if err == nil || !strings.Contains(err.Error(), "status 307") {
		t.Errorf("post through a redirect = %v, want a failed delivery with status 307", err)
	}
	if hits.Load() != 0 {
		t.Errorf("the redirect target saw %d requests, want none", hits.Load())
	}
}

// TestWebhookTextOmitsCheckOutput: the banner of a failed check carries
// the check's tail; the webhook's text for it says only that it failed.
func TestWebhookTextOmitsCheckOutput(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, ctx, filepath.Join(t.TempDir(), "store.db"))
	defer st.Close()
	seedProject(t, ctx, st)
	secretish := "printenv says " + strings.Repeat("x", 12)
	appendEvent(t, ctx, st, KindTaskCheckFailed, 1, 1, `{"status":"needs_foreman","reason":"`+secretish+`"}`)

	r := newRelay(t)
	banner := &Fake{}
	if err := NewLoop(st, banner, nil).WithWebhook(r.hook()).Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	waitFor(t, "the post", func() bool { return len(r.received()) >= 1 })
	if p := r.received()[0]; strings.Contains(p.Raw, secretish) {
		t.Errorf("webhook body %q carries the check output", p.Raw)
	}
	if got := banner.Notes(); len(got) != 1 || !strings.Contains(got[0].Body, secretish) {
		t.Errorf("banner notes = %v, want the desktop banner to keep the check tail", got)
	}
}
