package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The checked head and suspects (§7.1 "Gate evidence and batch holds").
// The runner reports, with every land_ready answer, what the command saw
// (landGateEvidence); the engine records each of its own pushes that moved
// a target (land_publications) and keeps the last verified green baseline
// (land_baselines). Attribution is conditional: a stable exit 1 of the
// recognized shipped script, checked against this target, names as
// suspects the members of the recorded publications after the baseline
// through the checked head, following the engine's own publication links.
// Without a baseline for the same remote and command identity, at a head
// brigada did not publish, across a gap in the recorded chain, or for a
// head the records show the target left and came back to, the suspects are
// unknown and no task is named. Any other command gates by its exit and
// supplies neither baseline nor suspects.

// suspectsUnknown is the suspect line when no task can be named.
const suspectsUnknown = "suspects unknown"

// landHeadAbsent is a runner's observation of a ref the remote lacks.
const landHeadAbsent = "absent"

// landGateEvidence is the runner's observation of one land_ready run.
type landGateEvidence struct {
	Command    string `json:"command"`
	Digest     string `json:"digest,omitempty"`
	Recognized bool   `json:"recognized,omitempty"`
	Remote     string `json:"remote"`
	Ref        string `json:"ref"`
	Attributed bool   `json:"attributed,omitempty"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Exit       int    `json:"exit"`
	Line       string `json:"line,omitempty"`
	Stable     bool   `json:"stable,omitempty"`
}

// landPublication is one push that moved the target, as a batch reports
// it: the head it moved from, the head it left and the members it
// published in landing order.
type landPublication struct {
	Predecessor string                  `json:"predecessor"`
	Final       string                  `json:"final"`
	Members     []landPublicationMember `json:"members"`
}

// landPublicationMember is one published member and its own commit.
type landPublicationMember struct {
	TaskID int64  `json:"task_id"`
	Commit string `json:"commit"`
}

// attributes reports whether the evidence speaks for the target's head
// (§7.1): the recognized shipped script, checking this very target, with
// heads stable around the run and the first fetch agreeing.
func (ev *landGateEvidence) attributes(target string) bool {
	return ev != nil && ev.Recognized && ev.Attributed && ev.Stable && ev.Ref == target &&
		ev.After != "" && ev.Before == ev.After
}

// landJobTargetTx is the target a task's land_ready answer is about: its
// recorded landing's target at the push phase, the project's land target
// otherwise.
func landJobTargetTx(tx *sql.Tx, project, taskID int64) (string, error) {
	if l, err := landingTx(tx, taskID); err != nil {
		return "", err
	} else if l != nil {
		return l.TargetBranch, nil
	}
	var defaultBranch string
	if err := tx.QueryRow("SELECT default_branch FROM projects WHERE id = ?", project).Scan(&defaultBranch); err != nil {
		return "", fmt.Errorf("read the default branch of project %d: %w", project, err)
	}
	return effectiveLandTargetTx(tx, project, defaultBranch)
}

// landHeadLeftTx reports whether a recorded publication moved the target
// off head (§7.1): the target sitting at head again means it moved away
// and back, which endpoint observations cannot see, so evidence at that
// head is invalidated.
func landHeadLeftTx(tx *sql.Tx, project int64, target, head string) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM land_publications
		WHERE project_id = ? AND target_branch = ? AND predecessor_head = ?`, project, target, head).Scan(&n); err != nil {
		return false, fmt.Errorf("read the publications from %s: %w", head, err)
	}
	return n > 0, nil
}

// landPublicationAtTx reads the latest recorded publication that left the
// target at head: its id, predecessor and members. ok is false when
// brigada published no such head.
func landPublicationAtTx(tx *sql.Tx, project int64, target, head string) (id int64, pred string, members []landPublicationMember, ok bool, err error) {
	var raw string
	err = tx.QueryRow(`SELECT id, predecessor_head, members FROM land_publications
		WHERE project_id = ? AND target_branch = ? AND final_head = ? ORDER BY id DESC LIMIT 1`, project, target, head).
		Scan(&id, &pred, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil, false, nil
	}
	if err != nil {
		return 0, "", nil, false, fmt.Errorf("read the publication of %s: %w", head, err)
	}
	if json.Unmarshal([]byte(raw), &members) != nil {
		members = nil
	}
	return id, pred, members, true, nil
}

// recordLandPublicationTx records one push of brigada's that moved the
// target (§7.1): its link from predecessor to final head, the job and the
// members it published. A push that moved nothing records nothing.
func recordLandPublicationTx(tx *sql.Tx, project int64, target string, job int64, pub landPublication, now time.Time) error {
	if pub.Predecessor == "" || pub.Final == "" || pub.Predecessor == pub.Final || len(pub.Members) == 0 {
		return nil
	}
	b, err := json.Marshal(pub.Members)
	if err != nil {
		return fmt.Errorf("encode the publication of job %d: %w", job, err)
	}
	if _, err := tx.Exec(`INSERT INTO land_publications (project_id, target_branch, predecessor_head, final_head, job_id, members, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, project, target, pub.Predecessor, pub.Final, nullIfZero(job), string(b), fmtTime(now)); err != nil {
		return fmt.Errorf("record the publication of job %d: %w", job, err)
	}
	return nil
}

// noteLandBaselineTx stores a green baseline from a land_ready pass
// (§7.1): only the recognized script's stable exit 0, attributed to this
// target, for an existing head that is one of brigada's recorded
// publications and that the records do not show the target leaving. An
// exit 0 for an absent branch permits landing and establishes nothing; so
// does any other command's 0. Call it before recording the job's own
// publication, which leaves the checked head.
func noteLandBaselineTx(tx *sql.Tx, project int64, target string, ev *landGateEvidence, now time.Time) error {
	if !ev.attributes(target) || ev.Exit != 0 || ev.After == landHeadAbsent {
		return nil
	}
	if left, err := landHeadLeftTx(tx, project, target, ev.After); err != nil || left {
		return err
	}
	pub, _, _, ok, err := landPublicationAtTx(tx, project, target, ev.After)
	if err != nil || !ok {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO land_baselines (project_id, target_branch, remote, command_path, command_digest, head, publication_id, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project_id, target_branch) DO UPDATE SET remote = excluded.remote, command_path = excluded.command_path,
			command_digest = excluded.command_digest, head = excluded.head, publication_id = excluded.publication_id,
			verified_at = excluded.verified_at`,
		project, target, ev.Remote, ev.Command, ev.Digest, ev.After, pub, fmtTime(now)); err != nil {
		return fmt.Errorf("record the land_ready baseline of %s: %w", target, err)
	}
	return nil
}

// landSuspectsTx words a hold's suspect line (§7.1): the members of the
// recorded publications after the baseline through the checked head, in
// landing order, the baseline's own members excluded and each task named
// once; "suspects unknown" with no task named when the evidence does not
// speak for the target, the target is known to have left the head and
// come back, there is no baseline for the same remote and command
// identity, or the chain from the checked head back to the baseline has a
// gap; and an explicit empty list at the baseline itself.
func landSuspectsTx(tx *sql.Tx, project int64, target string, ev *landGateEvidence) (string, error) {
	if !ev.attributes(target) || ev.After == landHeadAbsent {
		return suspectsUnknown, nil
	}
	if left, err := landHeadLeftTx(tx, project, target, ev.After); err != nil || left {
		return suspectsUnknown, err
	}
	var remote, path, digest, base string
	var basePub int64
	err := tx.QueryRow(`SELECT remote, command_path, command_digest, head, publication_id FROM land_baselines
		WHERE project_id = ? AND target_branch = ?`, project, target).Scan(&remote, &path, &digest, &base, &basePub)
	if errors.Is(err, sql.ErrNoRows) {
		return suspectsUnknown, nil
	}
	if err != nil {
		return "", fmt.Errorf("read the land_ready baseline of %s: %w", target, err)
	}
	if remote != ev.Remote || path != ev.Command || digest != ev.Digest {
		return suspectsUnknown, nil // a changed command identity or remote
	}
	short := base
	if len(short) > 12 {
		short = short[:12]
	}
	if ev.After == base {
		return "suspects: none, no recorded landings since the baseline " + short, nil
	}
	excluded := map[int64]bool{}
	var baseRaw string
	if err := tx.QueryRow("SELECT members FROM land_publications WHERE id = ?", basePub).Scan(&baseRaw); err == nil {
		var bm []landPublicationMember
		if json.Unmarshal([]byte(baseRaw), &bm) == nil {
			for _, m := range bm {
				excluded[m.TaskID] = true
			}
		}
	}
	// Walk the recorded links back from the checked head to the baseline.
	var chain [][]landPublicationMember
	seen := map[string]bool{}
	for head := ev.After; head != base; {
		if seen[head] {
			return suspectsUnknown, nil
		}
		seen[head] = true
		_, pred, members, ok, err := landPublicationAtTx(tx, project, target, head)
		if err != nil {
			return "", err
		}
		if !ok {
			return suspectsUnknown, nil // a gap: a head brigada did not publish
		}
		chain = append(chain, members)
		head = pred
	}
	named := map[int64]bool{}
	var words []string
	for i := len(chain) - 1; i >= 0; i-- {
		for _, m := range chain[i] {
			if excluded[m.TaskID] || named[m.TaskID] {
				continue
			}
			named[m.TaskID] = true
			words = append(words, fmt.Sprintf("task %d", m.TaskID))
		}
	}
	if len(words) == 0 {
		return "suspects: none, no recorded landings since the baseline " + short, nil
	}
	return "suspects since the baseline " + short + ": " + strings.Join(words, ", "), nil
}
