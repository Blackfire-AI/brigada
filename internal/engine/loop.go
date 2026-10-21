package engine

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The loop signals (§5.6), the repetition a stall watch cannot see: a
// worker that stays active repeating itself. Two detectors, one route —
// the task_loop event, the inbox and the wake a stall parks through,
// the reason naming the loop. Nothing is killed and no round is queued
// on the way: the foreman decides, as for a stall.

// loopRepeats is how many times the same check failure — the same
// failing test names, else the same fingerprint of the normalised
// output tail — repeats across a task's consecutive fix rounds before
// it counts as a loop (§5.6): the first failure plus this many repeats
// of it.
const loopRepeats = 2

// workerLoopTx routes one runner loop report to the foreman (§5.6):
// the same event path a stall parks through — the inbox row, the wake —
// with the loop as the reason. Once per job: the report rides every
// beat until the run ends, and one event is the whole signal. The task
// keeps running; the foreman decides what the loop means.
func (e *Engine) workerLoopTx(tx *sql.Tx, job *jobRow, call string, now time.Time) error {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM events WHERE kind = ? AND job_id = ? LIMIT 1`,
		evTaskLoop, job.ID).Scan(&one)
	if err == nil {
		return nil // already reported
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read the loop events of job %d: %w", job.ID, err)
	}
	reason := "the worker repeats the same tool call"
	if call != "" {
		reason += ": " + oneLine(call, needsYouReasonLimit)
	}
	if err := inboxEventTx(tx, evTaskLoop, job.ProjectID, job.TaskID, job.ID,
		eventPayload{Reason: boundNote(reason)}, now); err != nil {
		return err
	}
	return e.wakeForemanTx(tx, job.ProjectID, now)
}

// checkLoopTx reports whether the task's checks end in a loop (§5.6):
// the completion's own failed check repeating — the same fingerprint —
// across the loopRepeats failed checks before it, consecutive in the
// task's work and fix jobs since its latest restart (a fresh attempt
// changes the worker's conditions; its failures start a new chain). A
// check that passed, a job that never checked, a failure that changed:
// each breaks the chain. why is the loop event's reason line when the
// answer is yes.
func (e *Engine) checkLoopTx(tx *sql.Tx, job *jobRow, output string) (bool, string, error) {
	fp := checkFingerprint(output)
	if fp == "" {
		return false, "", nil
	}
	if job.Restart {
		return false, "", nil // a fresh attempt's first failure: the chain starts with it
	}
	rows, err := tx.Query(`SELECT check_result, COALESCE(restart, 0) FROM jobs
		WHERE task_id = ? AND kind IN ('work', 'fix') AND id < ?
		ORDER BY id DESC LIMIT ?`, job.TaskID, job.ID, loopRepeats)
	if err != nil {
		return false, "", fmt.Errorf("read the checks of task %d: %w", job.TaskID, err)
	}
	defer rows.Close()
	var texts []string
	var restarts []bool
	for rows.Next() {
		var check sql.NullString
		var restart int64
		if err := rows.Scan(&check, &restart); err != nil {
			return false, "", fmt.Errorf("read the checks of task %d: %w", job.TaskID, err)
		}
		if !check.Valid {
			break // a run that never checked: the chain ends
		}
		texts = append(texts, check.String)
		restarts = append(restarts, restart == 1)
	}
	if err := rows.Err(); err != nil {
		return false, "", fmt.Errorf("read the checks of task %d: %w", job.TaskID, err)
	}
	repeats := 0
	for i, check := range texts { // newest first
		co, ran := parseCheckResult(check)
		if !ran || (co.ExitCode == 0 && !co.TimedOut) {
			break // no failed check here: the chain ends
		}
		f := checkFingerprint(co.Output)
		if f == "" || f != fp {
			break // a different failure: no loop
		}
		repeats++
		if restarts[i] {
			break // the fresh attempt's own first failure: the chain's start
		}
	}
	if repeats < loopRepeats {
		return false, "", nil
	}
	return true, fmt.Sprintf("the same check failure across %d consecutive rounds after its first: %s",
		loopRepeats, loopFingerprintText(output)), nil
}

// loopFingerprintText words what made the failures one failure: the
// failing test names when the output carried them, else the
// fingerprint's short hash.
func loopFingerprintText(output string) string {
	if names := failingTestNames(output); len(names) > 0 {
		return "the failing tests " + strings.Join(names, ", ")
	}
	return "the same normalised check output (" + checkFingerprint(output) + ")"
}

// checkFingerprint names one failed check's failure (§5.6): its failing
// test names when the output carries them, else a short hash of the
// normalised output tail. "" when the check said nothing — nothing
// identifiable, nothing to compare.
func checkFingerprint(output string) string {
	if strings.TrimSpace(output) == "" {
		return ""
	}
	if names := failingTestNames(output); len(names) > 0 {
		return "tests:" + strings.Join(names, " ")
	}
	sum := sha256.Sum256([]byte(normaliseCheckTail(output)))
	return "tail:" + hex.EncodeToString(sum[:8])
}

// failingTestNames lists the output's failing test names, sorted and
// deduplicated (§5.6): go test's --- FAIL lines, pytest's FAILED lines.
var loopFailNames = []*regexp.Regexp{
	regexp.MustCompile(`--- FAIL: (\S+)`),
	regexp.MustCompile(`^FAILED (\S+)`),
}

func failingTestNames(output string) []string {
	seen := map[string]bool{}
	for _, re := range loopFailNames {
		for _, m := range re.FindAllStringSubmatch(output, -1) {
			if m[1] != "" {
				seen[m[1]] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// The normalisation a check's output tail goes through before it is
// fingerprinted (§5.6): what changes between two runs of the same
// failure — timings, durations, addresses, paths and line numbers —
// goes, so only the failure itself remains.
var (
	loopDurationRe = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|µs|μs|ms|s)\b`)
	loopAddressRe  = regexp.MustCompile(`0[xX][0-9a-fA-F]+`)
	loopIPv4Re     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	loopClockRe    = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}\b`)
	loopPathRe     = regexp.MustCompile(`(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+`)
	loopLineRe     = regexp.MustCompile(`:\d+(?::\d+)?\b`)
)

// normaliseCheckTail strips the volatile and collapses the whitespace
// of a check's output tail (§5.6).
func normaliseCheckTail(output string) string {
	for _, re := range []*regexp.Regexp{loopDurationRe, loopAddressRe, loopIPv4Re, loopClockRe, loopPathRe, loopLineRe} {
		output = re.ReplaceAllString(output, "")
	}
	return strings.Join(strings.Fields(output), " ")
}
