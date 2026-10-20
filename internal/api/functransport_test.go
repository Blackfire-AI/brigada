package api

import "net/http"

// funcTransport is an http.RoundTripper made of a function, so a test can
// route a Client's requests straight into a handler.
type funcTransport func(*http.Request) (*http.Response, error)

func (f funcTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
