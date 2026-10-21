package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// jobRow is a job as the engine reads it: NULL columns become zero
// values, and empty strings mean "no lease", "never leased", and so on.
type jobRow struct {
	ID             int64
	ProjectID      int64
	TaskID         int64 // 0 for a foreman turn
	Kind           Kind
	Key            string
	Mode           Mode
	Prompt         string
	Status         string
	AgentID        int64 // the agent the lease recorded; 0 until then
	ExcludeFamily  string
	ExcludeAgent   int64 // one agent the family rule's job must not run on (§7); 0 = none
	RequireAgent   int64 // the one agent this job must run on (§3); 0 = any
	LeaseID        string
	LeaseOwner     int64
	LeaseExpiresAt string
	LeaseEpoch     int64
	FenceToken     int64
	Followup       bool
	Attempts       int64
	Branch         string // the branch the engine named the job (§5.1); '' until its first lease
	Result         string
	FirstLeased    string
	LastLeased     string
	QueuedAt       string
	CapHeldAt      string // when a lease poll last found the job held by a full cap layer (§8.1)
	CapHeldLayer   string // the layer that held it; the job_cap_held event moves with a change
	CapHeldKey     string // the held instance: runner id, account label, model name or plan id (§12.2)
	LeasedSeconds  int64
	Harness        string
	Effort         string // the level passed to the CLI at lease time
	Model          string // a foreman turn's model; '' = none recorded (see foremanPairTx)
	Labels         string
	Restart        bool
	Retry          bool // a fix round's retry after a transient CLI failure (§3); never a restart
	ReleasedBy     int64
	ReleasedUntil  string
	ReleaseNote    string
	ReleasedLease  string
	DoneLease      string
	NotBefore      string // a queued job is not leasable before this time; '' = now (§3, §4.2)
}

// jobColumns reads a job whole; COALESCE turns NULL into the zero value
// so every scan is into plain fields.
const jobColumns = `id, project_id, COALESCE(task_id, 0), kind, key, mode, prompt, status,
	COALESCE(agent_id, 0), exclude_family, COALESCE(exclude_agent, 0), COALESCE(require_agent, 0),
	COALESCE(lease_id, ''), COALESCE(lease_owner, 0), COALESCE(lease_expires_at, ''), COALESCE(lease_epoch, 0),
	COALESCE(fence_token, 0), followup, attempts, COALESCE(branch, ''), COALESCE(result, ''),
	COALESCE(first_leased, ''), COALESCE(last_leased, ''), COALESCE(queued_at, ''),
	COALESCE(cap_held_at, ''), COALESCE(cap_held_layer, ''), COALESCE(cap_held_key, ''),
	COALESCE(leased_seconds, 0), harness, model, effort, labels, restart, retry,
	COALESCE(released_by, 0), COALESCE(released_until, ''), COALESCE(release_note, ''),
	COALESCE(released_lease, ''), COALESCE(done_lease, ''), COALESCE(not_before, '')`

// scanJob scans one job from a *sql.Row or *sql.Rows.
func scanJob(row interface{ Scan(dest ...any) error }) (*jobRow, error) {
	j := &jobRow{}
	err := row.Scan(&j.ID, &j.ProjectID, &j.TaskID, &j.Kind, &j.Key, &j.Mode, &j.Prompt, &j.Status,
		&j.AgentID, &j.ExcludeFamily, &j.ExcludeAgent, &j.RequireAgent,
		&j.LeaseID, &j.LeaseOwner, &j.LeaseExpiresAt, &j.LeaseEpoch,
		&j.FenceToken, &j.Followup, &j.Attempts, &j.Branch, &j.Result,
		&j.FirstLeased, &j.LastLeased, &j.QueuedAt,
		&j.CapHeldAt, &j.CapHeldLayer, &j.CapHeldKey,
		&j.LeasedSeconds, &j.Harness, &j.Model, &j.Effort, &j.Labels, &j.Restart, &j.Retry,
		&j.ReleasedBy, &j.ReleasedUntil, &j.ReleaseNote, &j.ReleasedLease,
		&j.DoneLease, &j.NotBefore)
	if err != nil {
		return nil, err
	}
	return j, nil
}

// loadJobTx reads one job by id; nil, nil when there is no such job.
func loadJobTx(tx *sql.Tx, id int64) (*jobRow, error) {
	j, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load job %d: %w", id, err)
	}
	return j, nil
}

// liveJobTx reads the live job holding the key, if any (§12.1: at most
// one live job per key).
func liveJobTx(tx *sql.Tx, key string) (*jobRow, error) {
	j, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+`
		FROM jobs WHERE key = ? AND status IN ('queued', 'leased', 'running')`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load live job for %s: %w", key, err)
	}
	return j, nil
}

// epochTx reads the store-wide fence epoch (§6.4).
func epochTx(tx *sql.Tx) (int64, error) {
	var epoch int64
	if err := tx.QueryRow("SELECT fence_epoch FROM meta WHERE id = 1").Scan(&epoch); err != nil {
		return 0, fmt.Errorf("read fence epoch: %w", err)
	}
	return epoch, nil
}

// jobLabels decodes the labels a runner must offer to lease the job.
func jobLabels(j *jobRow) []string {
	var labels []string
	if json.Unmarshal([]byte(j.Labels), &labels) != nil {
		return nil
	}
	return labels
}

// labelsCover reports whether the runner offers every label the job
// requires (§8).
func labelsCover(offered, required []string) bool {
	for _, want := range required {
		if !slices.Contains(offered, want) {
			return false
		}
	}
	return true
}

// affinityFreeTx reports whether a session-mode job for key is visible
// to owner: a live session pinned to another runner hides it (§8, §12.1).
func affinityFreeTx(tx *sql.Tx, key string, owner int64) (bool, error) {
	var runner int64
	var state string
	err := tx.QueryRow("SELECT COALESCE(runner_id, 0), state FROM sessions WHERE key = ?", key).Scan(&runner, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read session %s: %w", key, err)
	}
	return state != "active" || runner == 0 || runner == owner, nil
}

// usesKeySession reports whether a job runs in the session its key holds
// (§8): a session-mode job does, bound to the runner the session sits on —
// except a review. A re-review resumes its reviewer's own session, which
// never becomes the key's (the key's is the author's, and a fix round
// must find it where it was); the agent pin places it instead.
func usesKeySession(j *jobRow) bool {
	return j.Mode == ModeSession && j.Kind != KindReview
}

// upsertSessionTx records the key's session: which runner holds it and,
// when known, the harness session id the last run ended on (§4.2).
// An empty harnessSessionID keeps what is stored: leasing does not know
// it, completing does. A runner of 0 names no runner — the attach
// release's write-back, whose chat ran in the user's own terminal — and
// keeps the row's runner affinity: the id it writes is the key's, not
// this caller's, so the release must not hand the key to whoever leases
// next by wiping the runner the last lease pinned (§8).
func upsertSessionTx(tx *sql.Tx, key string, runner int64, harnessSessionID string, now time.Time) error {
	_, err := tx.Exec(`INSERT INTO sessions (key, runner_id, harness_session_id, state, last_used_at, created_at)
		VALUES (?, ?, ?, 'active', ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			runner_id = COALESCE(excluded.runner_id, sessions.runner_id),
			last_used_at = excluded.last_used_at,
			state = 'active',
			harness_session_id = CASE WHEN excluded.harness_session_id <> '' THEN excluded.harness_session_id ELSE sessions.harness_session_id END`,
		key, nullIfZero(runner), harnessSessionID, fmtTime(now), fmtTime(now))
	if err != nil {
		return fmt.Errorf("record session %s: %w", key, err)
	}
	return nil
}

// errorJSON is the engine's uniform failure record: every way a job can
// end up failed writes its reason here.
func errorJSON(msg string) string {
	b, err := json.Marshal(struct {
		Error string `json:"error"`
	}{msg})
	if err != nil {
		return `{"error":"unserializable failure"}`
	}
	return string(b)
}

// failJobTx fails a job with msg, banking accrual seconds of leased
// time. The lease id stays: it is what tells a late holder their own
// sweep, not another runner, ended the job. A failure ends the turn, so
// whatever wrote it — a hard cap, the queue TTL, lost runners — revokes
// the job's lease-scoped foreman token in the same transaction.
func failJobTx(tx *sql.Tx, id int64, msg string, accrue int64, now time.Time) error {
	_, err := tx.Exec(`UPDATE jobs SET status = 'failed', result = ?, lease_expires_at = NULL,
		last_leased = NULL, queued_at = NULL, leased_seconds = leased_seconds + ?, updated_at = ?
		WHERE id = ?`, errorJSON(msg), accrue, fmtTime(now), id)
	if err != nil {
		return fmt.Errorf("fail job %d: %w", id, err)
	}
	return revokeLeaseTokensTx(tx, id)
}

// accrualFrom returns the seconds of leased time between the lease
// start last and end; 0 when no lease was running (last empty) or when
// the timestamps do not parse, which a corrupt row must not turn into a
// failed sweep.
func accrualFrom(last string, end time.Time) int64 {
	if last == "" {
		return 0
	}
	start, err := parseTime(last)
	if err != nil || !end.After(start) {
		return 0
	}
	return int64(end.Sub(start) / time.Second)
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// isUniqueViolation reports whether err is a unique index refusing a
// write — for Submit, the one-live-job-per-key index (§12.1).
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE")
}
