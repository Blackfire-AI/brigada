package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The land_ready gate's evidence (§7.1 "The checked head and suspects").
// land_ready stays a general command — exit 0 means ready, not green CI;
// exit 1 means hold, not failed CI — and both call sites, the landing's
// and the push phase's, record what the command saw: its identity (path
// and content digest), the remote and the ref it checks, that ref's
// remote head immediately before and after the run, the raw exit and the
// first output line. Heads that differ, a present/absent transition or a
// failed lookup make the effective outcome not ready, whatever the exit;
// so does a first landing fetch that disagrees with the head after the
// run. Only the unmodified shipped scripts/land-ready.sh, recognized by
// its content digest, checks a ref the runner knows — brigada/landed —
// and its evidence is attributed to the landing target only when that is
// the target; any other command still gates by its exit and supplies no
// baseline or suspects.

// shippedLandReadyDigest is the SHA-256 of the unmodified shipped
// scripts/land-ready.sh; TestShippedLandReadyDigest keeps it in step.
const shippedLandReadyDigest = "44a6dcdca9e7189c6ae8144a109711faf980da25f907c09e4fc77b3f2b01cad7"

// shippedLandReadyRef is the branch the shipped script checks on the
// remote it runs against.
const shippedLandReadyRef = "brigada/landed"

// landHeadAbsent is the observation of a ref the remote does not have;
// "" is a lookup that failed.
const landHeadAbsent = "absent"

// landGateJSON is the gate's evidence a land record carries (§7.1).
type landGateJSON struct {
	Command    string `json:"command"`              // the repo-relative command path
	Digest     string `json:"digest,omitempty"`     // SHA-256 of its content, "" when it could not be read
	Recognized bool   `json:"recognized,omitempty"` // the unmodified shipped script
	Remote     string `json:"remote"`               // the remote the command ran against, redacted
	Ref        string `json:"ref"`                  // the branch whose head was observed
	Attributed bool   `json:"attributed,omitempty"` // the recognized script checked this landing's target
	Before     string `json:"before"`               // the head before the run: a commit, "absent", or "" for a failed lookup
	After      string `json:"after"`                // the head after the run, the same way
	Exit       int    `json:"exit"`                 // the raw exit
	Line       string `json:"line,omitempty"`       // the first output line
	Stable     bool   `json:"stable,omitempty"`     // both lookups succeeded and agree, and the first fetch agreed
}

// landGateIdentity reads the command's identity in the landing clone:
// its path, its content digest and whether it is the shipped script. The
// ref observed is the one the recognized script checks; for any other
// command the runner cannot know its ref, so the landing target's head is
// observed — its stability still guards the landing — and nothing is
// attributed.
func (r *jobRun) landGateIdentity(dir string) landGateJSON {
	grant := r.grant
	ev := landGateJSON{Command: grant.LandReady, Remote: redactURL(grant.RepoURL), Ref: grant.LandTarget}
	if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(grant.LandReady))); err == nil {
		sum := sha256.Sum256(b)
		ev.Digest = hex.EncodeToString(sum[:])
	}
	if ev.Digest == shippedLandReadyDigest {
		ev.Recognized = true
		ev.Ref = shippedLandReadyRef
		ev.Attributed = ev.Ref == grant.LandTarget
	}
	return ev
}

// landRemoteHead reads the remote head of the branch: its commit,
// landHeadAbsent when the remote answers cleanly that it has none, ""
// when it could not be asked.
func (r *jobRun) landRemoteHead(ctx context.Context, dir, branch string) string {
	out, err := r.landGit(ctx, dir, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return landHeadAbsent
	}
	return f[0]
}

// landFetchAgrees makes the first landing fetch of the target and
// reports whether it agrees with the head observed after the command
// (§7.1): an absent head must still be absent, a commit must be what the
// fetch brings. A disagreement — or a fetch that fails — means the
// command's answer was for another head, and it is not used.
func (r *jobRun) landFetchAgrees(ctx context.Context, dir, after string) (bool, string) {
	target := r.grant.LandTarget
	if after == landHeadAbsent {
		if now := r.landRemoteHead(ctx, dir, target); now != landHeadAbsent {
			return false, fmt.Sprintf("%s appeared on the remote after land_ready ran", target)
		}
		return true, ""
	}
	if err := r.landFetch(ctx, dir, target); err != nil {
		return false, err.Error()
	}
	got, err := r.landText(ctx, dir, "rev-parse", "refs/heads/"+target)
	if err != nil {
		return false, err.Error()
	}
	if got = strings.TrimSpace(got); got != after {
		return false, fmt.Sprintf("%s moved after land_ready ran: the command saw %s, the landing fetched %s", target, shortHead(after), shortHead(got))
	}
	return true, ""
}

// landHeadsUnstable words why the observations around the command make
// its answer unusable (§7.1), "" when they agree.
func landHeadsUnstable(ev *landGateJSON) string {
	switch {
	case ev.Before == "" || ev.After == "":
		return "could not read the remote head of " + ev.Ref + " around land_ready"
	case ev.Before != ev.After:
		return fmt.Sprintf("the remote head of %s moved while land_ready ran: %s before, %s after", ev.Ref, shortHead(ev.Before), shortHead(ev.After))
	}
	return ""
}

// shortHead abbreviates a commit for a reason line; "absent" stays.
func shortHead(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
