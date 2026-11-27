package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// The land_ready gate's evidence (§7.1 "The checked head and suspects").

// TestShippedLandReadyDigest keeps the recognized digest in step with
// the shipped script: a change to scripts/land-ready.sh is a new command
// identity, and its digest must be pinned here on purpose.
func TestShippedLandReadyDigest(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "land-ready.sh"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != shippedLandReadyDigest {
		t.Errorf("scripts/land-ready.sh digest = %s, want the pinned %s", got, shippedLandReadyDigest)
	}
}

// TestLandGateIdentityRecognizesTheShippedScript: the unmodified shipped
// script is recognized by its content, not its name; it checks
// brigada/landed and is attributed only to that target. A modified copy
// at the same path, or another command, is not recognized and observes
// the target for stability alone.
func TestLandGateIdentityRecognizesTheShippedScript(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "scripts", "land-ready.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body []byte) {
		if err := os.WriteFile(filepath.Join(dir, "scripts", "land-ready.sh"), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(shipped)
	for target, attributed := range map[string]bool{"brigada/landed": true, "main": false} {
		r := &jobRun{grant: &api.JobGrant{LandReady: "scripts/land-ready.sh", LandTarget: target, RepoURL: "file:///remote.git"}}
		ev := r.landGateIdentity(dir)
		if !ev.Recognized || ev.Ref != "brigada/landed" || ev.Attributed != attributed {
			t.Errorf("target %s: %+v, want recognized, checking brigada/landed, attributed %v", target, ev, attributed)
		}
	}
	write(append(append([]byte(nil), shipped...), []byte("\n# local edit\n")...))
	r := &jobRun{grant: &api.JobGrant{LandReady: "scripts/land-ready.sh", LandTarget: "brigada/landed"}}
	if ev := r.landGateIdentity(dir); ev.Recognized || ev.Attributed || ev.Ref != "brigada/landed" || ev.Digest == shippedLandReadyDigest {
		t.Errorf("modified script = %+v, want unrecognized, observing the target only", ev)
	}
}

// TestLandReadyMovedHeadIsNotReady: the remote head moving while the
// command runs makes its answer not ready whatever its exit — here 0 —
// keeping the raw exit, so nothing lands and nothing is held.
func TestLandReadyMovedHeadIsNotReady(t *testing.T) {
	for _, exit := range []string{"0", "1"} {
		t.Run("exit "+exit, func(t *testing.T) {
			remote := newRemote(t)
			mover := t.TempDir()
			gate := "git -C '" + mover + "' push -q origin HEAD:main >/dev/null 2>&1\necho 'CI is red'\nexit " + exit + "\n"
			seedGate(t, remote, gate)
			// The command's own side effect: a commit pushed onto main while
			// it runs, prepared on top of the seeded gate.
			git(t, "", "clone", "-q", "--branch", "main", remote, mover)
			if err := os.WriteFile(filepath.Join(mover, "moved.txt"), []byte("moved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, mover, "add", "-A")
			git(t, mover, "-c", "user.name=m", "-c", "user.email=m@brigada.invalid", "commit", "-qm", "move main")
			a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
			r, dir := batchRun(t, remote, engine.GateAuto, "", a)
			var bodies []string
			r.client = capturingClient(&bodies)
			r.grant.LandReady, r.cfg.CheckTimeout = ".brigada/land-ready.sh", 30*time.Second
			if r.runLandReady(context.Background(), dir) {
				t.Fatal("the gate let the landing go on though main moved under the command")
			}
			if !slices.ContainsFunc(bodies, func(s string) bool {
				return strings.Contains(s, `\"outcome\":\"not ready\"`) && strings.Contains(s, "moved while land_ready ran") &&
					strings.Contains(s, `\"exit\":`+exit)
			}) {
				t.Errorf("completions %q, want not ready naming the moved head with the raw exit %s", bodies, exit)
			}
		})
	}
}

// TestLandFetchAgreesWithThePostCommandHead: the first landing fetch
// must find the head the command saw; a target that moved after the
// command, or appeared where it was absent, disagrees.
func TestLandFetchAgreesWithThePostCommandHead(t *testing.T) {
	remote := newRemote(t)
	a := batchMember(t, remote, 1, "main", nil, map[string]string{"a.txt": "a\n"})
	r, dir := batchRun(t, remote, engine.GateAuto, "", a)
	ctx := context.Background()
	seen := r.landRemoteHead(ctx, dir, "main")
	if seen == "" || seen == landHeadAbsent {
		t.Fatalf("main's head = %q, want a commit", seen)
	}
	if ok, why := r.landFetchAgrees(ctx, dir, seen); !ok {
		t.Fatalf("the fetch disagreed with an unmoved head: %s", why)
	}
	pushToMain(t, remote, "late.txt", "late\n")
	if ok, why := r.landFetchAgrees(ctx, dir, seen); ok || !strings.Contains(why, "moved after land_ready ran") {
		t.Errorf("fetch after a move = %v, %q; want a disagreement", ok, why)
	}
	if got := r.landRemoteHead(ctx, dir, "no-such-branch"); got != landHeadAbsent {
		t.Errorf("an absent branch's head = %q, want %q", got, landHeadAbsent)
	}
	r.grant.LandTarget = "brigada/landed"
	if ok, _ := r.landFetchAgrees(ctx, dir, landHeadAbsent); !ok {
		t.Error("an absent target still absent disagreed")
	}
}

// seedGate commits .brigada/land-ready.sh with body onto the remote's main.
func seedGate(t *testing.T, remote, body string) {
	t.Helper()
	scratch := t.TempDir()
	git(t, "", "clone", "-q", "--branch", "main", remote, scratch)
	if err := os.MkdirAll(filepath.Join(scratch, ".brigada"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, ".brigada", "land-ready.sh"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, scratch, "add", "-A")
	git(t, scratch, "-c", "user.name=seed", "-c", "user.email=seed@brigada.invalid", "commit", "-qm", "seed the gate")
	git(t, scratch, "push", "-q", "origin", "main")
}
