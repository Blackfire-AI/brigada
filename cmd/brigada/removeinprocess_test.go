package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

type removalHandlerTransport struct{ handler http.Handler }

func (r removalHandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	r.handler.ServeHTTP(w, req)
	return w.Result(), nil
}

// Keep the socket tests, and repeat their command assertions over a real
// engine, authenticated API routes and client codec without opening a socket.
// This lets restrictive environments diagnose command failures too.
func TestRemoveInProcess(t *testing.T) {
	oldStart, oldClient, oldConnect := startRemovalTestEngine, newRemovalTestClient, connectRemoval
	t.Cleanup(func() {
		startRemovalTestEngine, newRemovalTestClient, connectRemoval = oldStart, oldClient, oldConnect
	})
	handlers := map[string]http.Handler{}
	startRemovalTestEngine = func(t *testing.T) (context.Context, string) {
		t.Helper()
		ctx := context.Background()
		state := testsupport.ShortStateDir(t)
		st, err := store.Open(ctx, ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		paths := lifecycle.PathsOf(state)
		if err := lifecycle.EnsureTokenFile(paths.Tokens); err != nil {
			t.Fatal(err)
		}
		srv, err := api.New(ctx, st, engine.New(st, engine.Config{}), nil, paths.Tokens)
		if err != nil {
			t.Fatal(err)
		}
		handlers[state] = srv.Handler()
		return ctx, state
	}
	clientFor := func(state, role string) (*api.Client, error) {
		_, token, err := lifecycle.TokenForRole(lifecycle.PathsOf(state).Tokens, role)
		if err != nil {
			return nil, err
		}
		return api.NewClientWithHTTPClient(&http.Client{
			Transport: removalHandlerTransport{handler: handlers[state]},
		}, token), nil
	}
	newRemovalTestClient = func(t *testing.T, _ context.Context, state string) *api.Client {
		t.Helper()
		client, err := clientFor(state, "admin")
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	connectRemoval = func(_ context.Context, rt *runtime, role string, _ bool, stderr io.Writer) (*api.Client, int) {
		client, err := clientFor(rt.stateDir, role)
		if err != nil {
			return nil, fail(stderr, err)
		}
		return client, 0
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"files", TestRemoveCommandAndDeleteConfig},
		{"live work", TestRemoveLiveWorkRefusesThenCancels},
		{"runner", TestRemoveConfirmationAndRunner},
		{"remote", TestRemoveRemoteOnlyPrintsDeletes},
		{"changed since plan", TestRemoveCommandGuardPrecedesArchive},
		{"work since plan", TestRemoveWorkArrivingSincePlanChangesNoFiles},
	} {
		t.Run(tc.name, tc.run)
	}
}
