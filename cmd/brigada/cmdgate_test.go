package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/approval"
	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/store"
	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// The gated commands against an in-process engine: each of plan approve
// and reject, land approve and reject and push approve prints the
// challenge, takes back the user's signature, refuses a wrong or reused
// one with its reason, and fails closed with no approval key enrolled.
// The signatures are real: made by `ssh-keygen -Y sign -f <key> -n
// brigada-approval <file holding the challenge's message>`. A host
// without ssh-keygen fails these tests, it does not skip them.

type gateCLI struct {
	t       *testing.T
	state   string
	st      *store.Store
	private string
	public  string
}

func newGateCLI(t *testing.T) *gateCLI {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Fatalf("ssh-keygen is required on the check host for the approval tests: %v", err)
	}
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
	srv, err := api.New(ctx, st, engine.New(st, engine.Config{ApprovalKeys: approval.StorePath(state)}), nil, paths.Tokens)
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

	private := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "", "-f", private).CombinedOutput(); err != nil {
		t.Fatalf("generate a key: %v: %s", err, out)
	}
	pub, err := os.ReadFile(private + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return &gateCLI{t: t, state: state, st: st, private: private, public: string(pub)}
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

// enroll runs `brigada approver add` for the test key.
func (g *gateCLI) enroll() {
	g.t.Helper()
	file := filepath.Join(g.t.TempDir(), "id.pub")
	if err := os.WriteFile(file, []byte(g.public), 0o600); err != nil {
		g.t.Fatal(err)
	}
	if code, _, errs := g.cli("approver", "add", "--label", "me", "--public-key", file); code != 0 {
		g.t.Fatalf("approver add: %d %s", code, errs)
	}
}

var challengeLineRE = regexp.MustCompile(`\(request #(\d+), challenge ([0-9a-f]{32}), expires`)

// pendingOf reads the request id and the message to sign out of the
// command's stderr, as the user following it would.
func pendingOf(t *testing.T, stderr string) (int64, string) {
	t.Helper()
	m := challengeLineRE.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("stderr names no pending challenge:\n%s", stderr)
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	var msg strings.Builder
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "    ") {
			msg.WriteString(strings.TrimPrefix(line, "    ") + "\n")
		}
	}
	return id, msg.String()
}

// signFile signs message with the test key and returns the .sig path.
func (g *gateCLI) signFile(message string) string {
	g.t.Helper()
	file := filepath.Join(g.t.TempDir(), "challenge")
	if err := os.WriteFile(file, []byte(message), 0o600); err != nil {
		g.t.Fatal(err)
	}
	if out, err := exec.Command("ssh-keygen", "-Y", "sign", "-f", g.private, "-n", approval.Namespace, file).CombinedOutput(); err != nil {
		g.t.Fatalf("sign: %v: %s", err, out)
	}
	return file + ".sig"
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

func TestGatedCommandsPrintTheChallengeAndTakeTheSignature(t *testing.T) {
	for _, c := range gateCases() {
		t.Run(c.name, func(t *testing.T) {
			g := newGateCLI(t)
			g.seed(c.planWait)
			if c.seed != nil {
				c.seed(g)
			}

			// Fail closed with no approval key enrolled: it says so, names the
			// enrollment command, prints the challenge, and approves nothing.
			code, out, errs := g.cli(c.args()...)
			if code != 1 || !strings.Contains(errs, approval.ErrNoKey.Error()) || !strings.Contains(errs, "brigada approver add") || out != "" {
				t.Fatalf("with no key: exit %d, stdout %q, stderr:\n%s", code, out, errs)
			}
			if c.ran(g) {
				t.Fatal("the act ran with no approval key")
			}

			g.enroll()
			code, _, errs = g.cli(c.args()...)
			request, message := pendingOf(t, errs)
			for _, want := range []string{
				"approval required: " + approval.ErrNoSignature.Error(), "signature wanted out of band",
				"ssh-keygen -Y sign -f <your private key> -n brigada-approval",
				fmt.Sprintf("--request %d --signature <file>.sig", request), "act: ",
			} {
				if code != 1 || !strings.Contains(errs, want) {
					t.Fatalf("the challenge: exit %d, stderr lacks %q:\n%s", code, want, errs)
				}
			}
			if c.ran(g) {
				t.Fatal("the act ran on a token alone")
			}

			// A wrong signature — a file that is not one — names its reason and
			// leaves the request to sign.
			junk := filepath.Join(t.TempDir(), "junk.sig")
			if err := os.WriteFile(junk, []byte("not a signature\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			req := strconv.FormatInt(request, 10)
			code, _, errs = g.cli(c.args("--request", req, "--signature", junk)...)
			if code != 1 || !strings.Contains(errs, approval.ErrBadSignature.Error()) || !strings.Contains(errs, "request #"+req) || c.ran(g) {
				t.Fatalf("a wrong signature: exit %d, ran %v, stderr:\n%s", code, c.ran(g), errs)
			}

			// The user's real signature proceeds.
			sig := g.signFile(message)
			code, out, errs = g.cli(c.args("--request", req, "--signature", sig)...)
			if code != 0 || errs != "" {
				t.Fatalf("the signed command: exit %d, stdout %q, stderr %q", code, out, errs)
			}
			if !c.ran(g) {
				t.Fatalf("the act did not run after a verified signature; stdout %q", out)
			}

			// And the same signature a second time is refused.
			if code, _, errs := g.cli(c.args("--request", req, "--signature", sig)...); code == 0 {
				t.Errorf("a reused signature passed: %s", errs)
			}
			if st := g.str("SELECT state FROM approval_requests WHERE id = " + req); st != "signed" {
				t.Errorf("the request is %s, want signed", st)
			}
		})
	}
}

func (g *gateCLI) str(query string) string {
	g.t.Helper()
	var s string
	if err := g.st.View(context.Background(), func(tx *sql.Tx) error { return tx.QueryRow(query).Scan(&s) }); err != nil {
		g.t.Fatalf("query %q: %v", query, err)
	}
	return s
}

// A signature made for one act does not approve another: the plan's
// challenge signed and presented on the other plan's request is refused,
// and the other plan stays where it was.
func TestGatedCommandRefusesAnotherActsSignature(t *testing.T) {
	g := newGateCLI(t)
	g.seed("pending_approval")
	g.exec(`INSERT INTO plans (project_id, title, state, created_at, updated_at) VALUES (1, 'second', 'pending_approval', ?, ?)`, gateTS, gateTS)
	g.enroll()
	_, _, one := g.cli("plan", "approve", "1")
	_, _, two := g.cli("plan", "approve", "2")
	reqOne, msgOne := pendingOf(t, one)
	reqTwo, _ := pendingOf(t, two)
	if reqOne == reqTwo {
		t.Fatal("two plans share a request")
	}
	sig := g.signFile(msgOne)
	code, _, errs := g.cli("plan", "approve", "--request", strconv.FormatInt(reqTwo, 10), "--signature", sig, "2")
	if code != 1 || !strings.Contains(errs, approval.ErrBadSignature.Error()) {
		t.Fatalf("plan 1's signature on plan 2: exit %d, stderr:\n%s", code, errs)
	}
	if n := g.count("SELECT COUNT(*) FROM plans WHERE state = 'pending_approval'"); n != 2 {
		t.Errorf("%d plans still awaiting, want 2", n)
	}
	// And the request that names plan 1 cannot be answered for plan 2.
	code, _, errs = g.cli("plan", "approve", "--request", strconv.FormatInt(reqOne, 10), "--signature", sig, "2")
	if code != 1 || !strings.Contains(errs, approval.ErrWrongAct.Error()) {
		t.Fatalf("plan 1's request on plan 2: exit %d, stderr:\n%s", code, errs)
	}
}
