package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// webhookTimeout caps one POST attempt when the caller's context names
// no sooner deadline: the webhook is fire-and-forget, and a relay that
// never answers must not hold the delivery loop's pass — or the
// engine's tick or idle exit behind it.
const webhookTimeout = 5 * time.Second

// webhookRetries is how many times a failed POST is tried again: one.
// The webhook is a mirror, not a channel with delivery guarantees; a
// second failure is logged and the notification is gone.
const webhookRetries = 1

// webhookBody is the JSON one POST carries (§10): the same moment the
// desktop banner fires, small and plain so ntfy, Slack or a Telegram
// bot relay can take it as-is — no per-service adapters. Project is the
// repo URL; the ids are 0 when the event names none; time is the log's
// RFC 3339 stamp.
type webhookBody struct {
	Project string `json:"project"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	Task    int64  `json:"task"`
	Plan    int64  `json:"plan"`
	Time    string `json:"time"`
}

// Webhook POSTs one small JSON document per delivered notification to a
// single URL (§10), set by the config file's notify.webhook — never by
// the project's own files, for the URL may carry a token. The URL is
// the one place the token lives: errors are stripped of it before they
// surface, so it never reaches a log line, the status or the history.
type Webhook struct {
	// URL is the relay's http or https address.
	URL string
	// Client posts; nil uses the default. Tests inject one.
	Client *http.Client
	// Timeout caps one attempt; zero keeps webhookTimeout.
	Timeout time.Duration
}

// ValidWebhookURL reports whether raw is an http or https URL with a
// host — the shape a relay publishes. The config reader refuses
// anything else, so a typo cannot point notifications nowhere.
func ValidWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("want an http or https URL")
	}
	return nil
}

// Post delivers one notification's JSON for ev, body being the same
// one-line text the banner shows. A failed attempt is retried once —
// unless ctx already ended, when there is nothing to retry against.
// The returned error names no URL, so logging it leaks nothing.
func (w *Webhook) Post(ctx context.Context, ev Event, body string) error {
	buf, err := json.Marshal(webhookBody{
		Project: ev.Project,
		Kind:    ev.Kind,
		Text:    body,
		Task:    ev.TaskID,
		Plan:    ev.PlanID,
		Time:    ev.At,
	})
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = webhookTimeout
	}
	var last error
	for attempt := 0; attempt <= webhookRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return scrubURL(err)
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		err := w.postOnce(attemptCtx, client, buf)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			// The loop's context ended — shutdown or the pass budget —
			// not the relay: retrying against a dead context answers at
			// once, so stop here.
			return scrubURL(err)
		}
		last = err
	}
	return last
}

// postOnce sends one POST and reads the answer just enough to know it:
// any 2xx is delivery, everything else is failure with the status as
// the whole message.
func (w *Webhook) postOnce(ctx context.Context, client *http.Client, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return scrubURL(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return scrubURL(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: status %d", resp.StatusCode)
	}
	return nil
}

// scrubURL prefixes the webhook's name onto the failure after scrubCause
// has stripped it: what reaches a log line names the failure, never the
// address.
func scrubURL(err error) error {
	return fmt.Errorf("webhook: %w", scrubCause(err))
}

// scrubCause unwraps the net stack's quoting layers: a *url.Error quotes
// the full URL — token included — a *net.OpError beneath it the address
// it reached, and a *net.DNSError the host it looked up. Only the
// cause's own words survive.
func scrubCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		return errors.New("dns lookup failed")
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return fmt.Errorf("%s: %w", oe.Op, scrubCause(oe.Err))
	}
	return err
}
