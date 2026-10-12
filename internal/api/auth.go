package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/store"
)

// The token roles (§12.1). A foreman token is scoped to one project,
// written as `foreman:<project id>` in the token file.
const (
	roleClient  = "client"
	roleForeman = "foreman"
	roleRunner  = "runner"
	roleAdmin   = "admin"
)

// token is one entry of the token file: the name a caller is known by
// (a runner's identity, §12.3), its role and, for a foreman, its
// project. Only the sha256 of the secret is kept — never the secret.
type token struct {
	name    string
	role    string
	project int64 // foreman only; 0 = unscoped
	sha     [sha256.Size]byte
}

// identity is the authenticated caller behind a request.
type identity struct {
	name    string
	role    string
	project int64 // foreman only
}

// mayTouchProject reports whether the identity may touch things that
// belong to project. Every role but the foreman is unscoped; a foreman
// token is walled into its own project (§12.1).
func (id identity) mayTouchProject(project int64) bool {
	return id.role != roleForeman || id.project == project
}

// loadTokenFile reads the token file: one `name role[:project] secret`
// per line, `#` comments and blank lines allowed (§6.4, §12.1). The
// file must be mode 0600 or tighter — it holds the secrets everyone
// authenticates with.
func loadTokenFile(path string) ([]token, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("token file %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("token file %s: mode %04o is looser than 0600", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("token file %s: %w", path, err)
	}
	var tokens []token
	seenName := map[string]bool{}
	seenHash := map[[sha256.Size]byte]bool{}
	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("token file %s: line %d: want `name role[:project] secret`, got %q", path, n+1, line)
		}
		name, roleSpec, secret := fields[0], fields[1], fields[2]
		if seenName[name] {
			return nil, fmt.Errorf("token file %s: line %d: duplicate name %q", path, n+1, name)
		}
		tok := token{name: name, sha: sha256.Sum256([]byte(secret))}
		if scope, ok := strings.CutPrefix(roleSpec, "foreman:"); ok {
			tok.role = roleForeman
			id, err := strconv.ParseInt(scope, 10, 64)
			if err != nil || id <= 0 {
				return nil, fmt.Errorf("token file %s: line %d: a foreman token is scoped as `foreman:<project id>`, got %q", path, n+1, roleSpec)
			}
			tok.project = id
		} else {
			switch roleSpec {
			case roleClient, roleRunner, roleAdmin:
				tok.role = roleSpec
			case roleForeman:
				return nil, fmt.Errorf("token file %s: line %d: a foreman token is scoped as `foreman:<project id>`", path, n+1)
			default:
				return nil, fmt.Errorf("token file %s: line %d: unknown role %q; want client, foreman:<project id>, runner or admin", path, n+1, roleSpec)
			}
		}
		if seenHash[tok.sha] {
			return nil, fmt.Errorf("token file %s: line %d: the same secret appears twice", path, n+1)
		}
		seenName[name] = true
		seenHash[tok.sha] = true
		tokens = append(tokens, tok)
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("token file %s: no tokens in it", path)
	}
	return tokens, nil
}

// syncTokens re-writes the store's tokens table from the file (§6.4):
// only the sha256 hex reaches the store — the secret never does — with
// the role and, for a foreman, the project. A scope naming a project
// the store does not know yet lands as NULL: the file, not the store,
// is what authentication checks, and projects may be created after the
// engine first starts.
func syncTokens(ctx context.Context, st *store.Store, tokens []token) error {
	return st.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM tokens"); err != nil {
			return fmt.Errorf("clear tokens: %w", err)
		}
		for _, t := range tokens {
			if _, err := tx.Exec(`INSERT INTO tokens (role, token_hash, project_id, created_at)
				VALUES (?, ?, (SELECT id FROM projects WHERE id = ?), ?)`,
				t.role, hex.EncodeToString(t.sha[:]), t.project, nowText()); err != nil {
				return fmt.Errorf("store token %q: %w", t.name, err)
			}
		}
		return nil
	})
}

// authenticate maps the request's bearer secret to an identity. The
// secret is hashed and every stored hash is compared in constant time,
// with no early exit, so timing does not reveal which entry matched.
// A missing, malformed or unknown token fails closed.
func (s *Server) authenticate(r *http.Request) (identity, bool) {
	const scheme = "bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) <= len(scheme) || !strings.EqualFold(auth[:len(scheme)], scheme) {
		return identity{}, false
	}
	digest := sha256.Sum256([]byte(auth[len(scheme):]))
	hit := -1
	for i := range s.tokens {
		if subtle.ConstantTimeCompare(digest[:], s.tokens[i].sha[:]) == 1 {
			hit = i
		}
	}
	if hit < 0 {
		return identity{}, false
	}
	t := &s.tokens[hit]
	return identity{name: t.name, role: t.role, project: t.project}, true
}

// handler is an endpoint that knows who is calling.
type handler func(w http.ResponseWriter, r *http.Request, id identity)

// authed wraps a handler behind bearer authentication and a role check:
// a missing or unknown token is 401, a known token in the wrong role is
// 403. Empty roles admits any authenticated caller.
func (s *Server) authed(roles []string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="brigada"`)
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing or unknown token")
			return
		}
		if len(roles) > 0 && !slices.Contains(roles, id.role) {
			writeError(w, http.StatusForbidden, codeForbidden,
				fmt.Sprintf("role %q may not call this endpoint", id.role))
			return
		}
		h(w, r, id)
	}
}

// withRunner resolves the runner's row: a runner token names the
// runner, and every runner call but register needs the row to exist
// (§5.1) — the row's id is what a lease references.
func (s *Server) withRunner(h func(w http.ResponseWriter, r *http.Request, runnerID int64)) handler {
	return func(w http.ResponseWriter, r *http.Request, id identity) {
		runnerID, err := s.eng.RunnerID(r.Context(), id.name)
		if err != nil {
			s.writeInternal(w, r, "resolve runner "+id.name, err)
			return
		}
		if runnerID == 0 {
			writeError(w, http.StatusForbidden, codeUnknownRunner,
				"unknown runner "+id.name+"; register first")
			return
		}
		h(w, r, runnerID)
	}
}
