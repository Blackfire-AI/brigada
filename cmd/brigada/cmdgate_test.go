package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The decision commands against an in-process engine: each of plan approve
// and reject, land approve and reject and push approve runs with a plain
// client token and makes its decision.

type gateCLI struct {
	t     *testing.T
	state string
	st    *store.Store
}

func newGateCLI(t *testing.T) *gateCLI {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	ctx := context.Background()
	state := testsupport.ShortStateDir(t)
	paths := lifecycle.PathsOf(state)
	st := openFreshStore(t, paths.Store)
	t.Cleanup(func() { st.Close() })
	if err := lifecycle.EnsureTokenFile(paths.Tokens); err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(ctx, st, engine.New(st, engine.Config{}), nil, paths.Tokens)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()
	old := connectGate
	connectGate = func(_ context.Context, rt *runtime, role string, _ bool, stderr io.Writer) (*api.Client, int) {
		_, secret, err := lifecycle.TokenForRole(lifecycle.PathsOf(rt.stateDir).Tokens, role)
		if err != nil {
			return nil, fail(stderr, err)
		}
		return api.NewClientWithHTTPClient(&http.Client{Transport: removalHandlerTransport{handler: handler}}, secret), 0
	}
	t.Cleanup(func() { connectGate = old })
	return &gateCLI{t: t, state: state, st: st}
}

func (g *gateCLI) exec(query string, args ...any) {
	g.t.Helper()
	if err := g.st.Update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	}); err != nil {
		g.t.Fatalf("exec %q: %v", query, err)
	}
}

func (g *gateCLI) count(query string) int {
	g.t.Helper()
	var n int
	if err := g.st.View(context.Background(), func(tx *sql.Tx) error { return tx.QueryRow(query).Scan(&n) }); err != nil {
		g.t.Fatalf("query %q: %v", query, err)
	}
	return n
}

const gateTS = "2026-11-09T00:00:00Z"
const gateRepo = "https://example.invalid/one.git"

// seed puts a project, a plan and a task in place: the plan awaiting
// approval or active.
func (g *gateCLI) seed(planState string) {
	g.exec(`INSERT INTO projects (repo_url, default_branch, settings, foreman_harness, foreman_model, created_at, updated_at)
		VALUES (?, 'main', '{}', 'claude', 'sonnet', ?, ?)`, gateRepo, gateTS, gateTS)
	g.exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at) VALUES (1, 'a plan', ?, ?, ?)`, planState, gateTS, gateTS)
	g.exec(`INSERT INTO tasks (plan_id, title, spec, check_cmd, class, state, base_ref, caps, created_at, updated_at)
		VALUES (1, 'a task', 'spec', 'go test ./...', 'implement', 'ready', 'main', '{}', ?, ?)`, gateTS, gateTS)
}

// cli runs a brigada command against the state directory.
func (g *gateCLI) cli(args ...string) (int, string, string) {
	g.t.Helper()
	var out, errb bytes.Buffer
	// The state directory goes before the flags and the positional
	// argument, which must come last.
	full := append(append([]string{}, args[:2]...), "--state-dir", g.state)
	code := run(context.Background(), append(full, args[2:]...), &out, &errb)
	return code, out.String(), errb.String()
}

type gateCase struct {
	name     string
	prefix   []string // the command and its subcommand
	tail     []string // after the flags: the positional argument
	planWait string   // the plan's state before: awaiting or active
	seed     func(g *gateCLI)
	ran      func(g *gateCLI) bool
}

func gateCases() []gateCase {
	hold := func(g *gateCLI) {
		g.exec(`INSERT INTO land_holds (task_id, reason, state, created_at) VALUES (1, 'heavy', 'held', ?)`, gateTS)
	}
	return []gateCase{
		{"plan approve", []string{"plan", "approve"}, []string{"1"}, "pending_approval", nil,
			func(g *gateCLI) bool {
				return g.count("SELECT COUNT(*) FROM plans WHERE id = 1 AND state = 'active'") == 1
			}},
		{"plan reject", []string{"plan", "reject"}, []string{"1"}, "pending_approval", nil,
			func(g *gateCLI) bool {
				return g.count("SELECT COUNT(*) FROM plans WHERE id = 1 AND state = 'cancelled'") == 1
			}},
		{"land approve", []string{"land", "approve"}, []string{"1"}, "active", hold,
			func(g *gateCLI) bool {
				return g.count("SELECT COUNT(*) FROM land_holds WHERE state = 'held'") == 0 && g.count("SELECT COUNT(*) FROM land_holds WHERE state = 'rejected'") == 0
			}},
		{"land reject", []string{"land", "reject"}, []string{"1"}, "active", hold,
			func(g *gateCLI) bool { return g.count("SELECT COUNT(*) FROM land_holds WHERE state = 'rejected'") == 1 }},
		{"push approve", []string{"push", "approve", "--repo", gateRepo}, nil, "active",
			func(g *gateCLI) {
				g.exec(`INSERT INTO landings (task_id, project_id, target_branch, commit_sha, pushed, created_at)
					VALUES (1, 1, 'main', 'abc123', 'waiting', ?)`, gateTS)
			},
			func(g *gateCLI) bool {
				return g.count("SELECT COUNT(*) FROM jobs WHERE task_id = 1 AND kind = 'land' AND status = 'queued'") == 1
			}},
	}
}

// args builds the command line: flags before the positional argument.
func (c gateCase) args(flags ...string) []string {
	out := append([]string{}, c.prefix...)
	out = append(out, flags...)
	return append(out, c.tail...)
}

// TestDecisionCommandsRunWithAClientToken: each decision command makes its
// decision in one call, with no further step.
func TestDecisionCommandsRunWithAClientToken(t *testing.T) {
	for _, c := range gateCases() {
		t.Run(c.name, func(t *testing.T) {
			g := newGateCLI(t)
			g.seed(c.planWait)
			if c.seed != nil {
				c.seed(g)
			}
			code, out, errs := g.cli(c.args()...)
			if code != 0 || errs != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
			}
			if !c.ran(g) {
				t.Fatalf("the decision did not run; stdout %q", out)
			}
		})
	}
}
