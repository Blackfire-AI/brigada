package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// maxBody caps every request body at 1 MiB: the API carries job specs
// and prompts, not artifacts, and a capped read keeps a runaway caller
// from pinning the engine's memory (§12.1).
const maxBody = 1 << 20

// The error codes the API answers with, one per HTTP status it uses.
const (
	codeUnauthorized     = "unauthorized"
	codeForbidden        = "forbidden"
	codeUnknownRunner    = "unknown_runner"
	codeNotFound         = "not_found"
	codeBadRequest       = "bad_request"
	codeIdemConflict     = "idempotency_conflict"
	codeTooLarge         = "payload_too_large"
	codeMethodNotAllowed = "method_not_allowed"
	codeInternal         = "internal"
)

// Listen serves the API on the Unix socket at socketPath (§12.1) until
// ctx is canceled or serving fails, then removes the socket file it
// owns. A leftover socket file is replaced only when it is provably
// stale — a socket nothing answers on — and a live engine's socket is
// never stolen. On cancel, in-flight requests get a bounded grace
// period to finish before the listener closes.
func (s *Server) Listen(ctx context.Context, socketPath string) error {
	l, err := listenSocket(socketPath)
	if err != nil {
		return err
	}
	defer os.Remove(socketPath)
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			srv.Close() // the grace ran out; cut the rest loose
		}
		<-serveErr // ErrServerClosed: the listener is closed, not failed
		return nil
	case err := <-serveErr:
		return fmt.Errorf("serve %s: %w", socketPath, err)
	}
}

// shutdownGrace is how long a canceled Listen lets in-flight requests
// finish before it closes their connections.
const shutdownGrace = 5 * time.Second

// listenSocket binds the Unix socket at path, mode 0600: only the
// engine's own user may talk to it.
//
// Nothing but a provably stale socket is ever removed or replaced: a
// non-socket file at path is refused outright, and a socket is stale
// only when connecting to it is refused (a dead engine's leftover) or
// it vanished on its own. A socket that answers is live, and a dial
// that neither connects nor is refused — a timeout, a permission error
// — cannot prove staleness, so it is refused too.
//
// The socket is never briefly world-accessible: it is bound under a
// freshly created 0700 directory (net.Listen itself creates the file
// 0777 masked only by the umask), tightened to 0600 there, and only
// then renamed onto path — an atomic replacement that also disposes of
// a stale leftover in one step.
func listenSocket(path string) (net.Listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("socket path %s: %s, not a Unix socket; refusing to remove or bind over it", path, info.Mode())
		}
		if err := refuseUnlessStale(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat socket %s: %w", path, err)
	}

	dir, err := os.MkdirTemp(filepath.Dir(path), ".brigada-sock-*")
	if err != nil {
		return nil, fmt.Errorf("create the private socket dir near %s: %w", path, err)
	}
	defer os.RemoveAll(dir)
	staged := filepath.Join(dir, "sock")
	l, err := net.Listen("unix", staged)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", staged, err)
	}
	if err := os.Chmod(staged, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("chmod socket %s to 0600: %w", staged, err)
	}
	// The stale probe above can race another engine binding path in the
	// meantime; look once more, so the rename that follows can never
	// steal a live engine's socket.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 && answersOn(path) {
		l.Close()
		return nil, fmt.Errorf("socket %s: another engine is listening on it; refusing to steal it", path)
	}
	if err := os.Rename(staged, path); err != nil {
		l.Close()
		return nil, fmt.Errorf("move the socket onto %s: %w", path, err)
	}
	return l, nil
}

// refuseUnlessStale returns nil when the socket at path is provably
// stale, and the reason not to touch it otherwise.
func refuseUnlessStale(path string) error {
	conn, err := net.DialTimeout("unix", path, time.Second)
	switch {
	case err == nil:
		conn.Close()
		return fmt.Errorf("socket %s: another engine is listening on it; refusing to steal it", path)
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, os.ErrNotExist):
		return nil // a dead engine's leftover, or it vanished mid-probe
	default:
		return fmt.Errorf("socket %s: cannot prove it is stale (%v); refusing to remove it", path, err)
	}
}

// answersOn reports whether anything accepts connections on the Unix
// socket at path.
func answersOn(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// writeJSON writes v as the response body.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// The encoder cannot fail on these shapes; if it somehow does, the
	// status is already sent and the body simply ends.
	json.NewEncoder(w).Encode(v)
}

// writeError answers in the API's uniform error shape.
func writeError(w http.ResponseWriter, code int, apiCode, msg string) {
	writeJSON(w, code, struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{msg, apiCode})
}

// writeInternal answers 500 for an engine or store failure. The error's
// text goes to the log, not the response: it may name paths or rows.
func (s *Server) writeInternal(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.Error("api: "+what+" failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, codeInternal, what+" failed")
}

// answerBody is the engine's protocol answers as the API returns them:
// ok, lost, gone, cancelled, stale_epoch and their kin are results, not
// HTTP errors (§12.3).
type answerBody struct {
	Answer string `json:"answer"`
}

// requireMethod answers 405 for anything but method, so every endpoint
// reports the same JSON error shape whatever the caller sent.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
			"method "+r.Method+" is not allowed here; want "+method)
		return false
	}
	return true
}

// readBody reads the request body up to maxBody. Over the cap it
// answers 413 itself and reports false.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, codeTooLarge,
			fmt.Sprintf("request body over the %d byte cap", maxBody))
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "read body: "+err.Error())
		return nil, false
	}
	return body, true
}

// decodeJSON reads the body under the cap and unmarshals it into v,
// answering 400 for anything that is not JSON.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, codeBadRequest, "malformed JSON body: "+err.Error())
		return false
	}
	return true
}
