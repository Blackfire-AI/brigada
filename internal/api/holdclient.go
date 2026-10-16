package api

import (
	"context"
	"net/http"
	"time"
)

// HoldAcquireRequest contains bridge observations, not model-supplied authority.
type HoldAcquireRequest = acquireRequest

func (c *Client) AcquireHold(ctx context.Context, req HoldAcquireRequest) (AcquireResponse, error) {
	var out AcquireResponse
	err := c.call(ctx, http.MethodPost, "/v1/foreman/hold/acquire", req, &out)
	return out, err
}

// HoldClient shares the transport, keeping the hold secret and fence immutable.
// The bootstrap client is never handed to a foreman tool.
func (c *Client) HoldClient(secret string, fence int64) *Client {
	return &Client{http: c.http, token: secret, fence: fence}
}

func (c *Client) HeartbeatHold(ctx context.Context) (time.Time, error) {
	var out struct {
		LeaseExpires time.Time `json:"lease_expires_at"`
	}
	err := c.call(ctx, http.MethodPost, "/v1/foreman/hold/heartbeat", struct{}{}, &out)
	return out.LeaseExpires, err
}

func (c *Client) YieldHold(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/foreman/hold/yield", struct{}{}, nil)
}

// ProjectReader defaults the read methods to the selected project. The bridge
// only passes it to read-only tools; mutation authority still requires a hold.
func (c *Client) ProjectReader(project int64) *Client {
	return &Client{http: c.http, token: c.token, readProject: project}
}
