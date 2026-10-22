package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/foreman"
)

// defaultForemanRotateTokens is foreman_rotate_tokens' default (§4.2): a
// resumed foreman session rereads its whole context every turn, so past
// this many input tokens on one turn the next one starts fresh.
const defaultForemanRotateTokens = 400000

// The digest's bounds (§4.2): a fresh session's first prompt says where
// the project stands in about 3k tokens at most, and a list cut short
// says how much it left out.
const (
	digestMaxPlans     = 8
	digestMaxTasks     = 20 // per plan
	digestMaxNeedsYou  = 10
	digestHistoryItems = 10
	digestLineLimit    = 160
	digestMaxBytes     = 10000
)

// foremanRotateTokensTx reads the project's rotation threshold: the
// setting when it is present, 0 included (never rotate), else the
// default.
func foremanRotateTokensTx(tx *sql.Tx, project int64) (int64, error) {
	var raw string
	if err := tx.QueryRow("SELECT settings FROM projects WHERE id = ?", project).Scan(&raw); err != nil {
		return 0, fmt.Errorf("read the settings of project %d: %w", project, err)
	}
	if raw != "" {
		var present map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &present); err != nil {
			return 0, fmt.Errorf("parse the settings of project %d: %w", project, err)
		}
		if v, has := present[SettingForemanRotateTokens]; has {
			if n, ok := rawInt(v); ok {
				return int64(n), nil
			}
		}
	}
	return defaultForemanRotateTokens, nil
}

// rotateForemanSessionTx starts the foreman's session over when it grew
// too large (§4.2): the last finished turn of the current generation
// read more input tokens (input plus cached) than the project's
// foreman_rotate_tokens. It is the harness switch's move — the key's
// session id cleared and the generation bumped, so a turn leased under
// the old one never writes its session back — with an event saying why.
// The input count is the adapter's, which includes cached reads.
// Never while a turn is live or a chat is attached, never on unknown
// usage, never with the setting at 0. Reports whether it rotated.
func (e *Engine) rotateForemanSessionTx(tx *sql.Tx, project int64, now time.Time) (bool, error) {
	threshold, err := foremanRotateTokensTx(tx, project)
	if err != nil || threshold <= 0 {
		return false, err
	}
	key := fmt.Sprintf("foreman:%d", project)
	live, err := liveJobTx(tx, key)
	if err != nil || live != nil {
		return false, err
	}
	var held int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM attach_holds WHERE project_id = ? AND state = 'live' AND expires_at > ?`,
		project, fmtTime(now)).Scan(&held); err != nil {
		return false, fmt.Errorf("read the attach holds of project %d: %w", project, err)
	}
	if held > 0 {
		return false, nil
	}
	var gen int64
	if err := tx.QueryRow("SELECT foreman_session_gen FROM projects WHERE id = ?", project).Scan(&gen); err != nil {
		return false, fmt.Errorf("read the foreman session generation of project %d: %w", project, err)
	}
	// Only a turn of the current generation speaks for the session: the
	// turn that crossed the threshold belongs to the generation it ended,
	// so it rotates once.
	var in, cached sql.NullInt64
	err = tx.QueryRow(`SELECT input_tokens, cached_input_tokens FROM jobs
		WHERE project_id = ? AND kind = ? AND session_gen = ? AND status IN (?, ?, ?)
		ORDER BY id DESC LIMIT 1`, project, KindForeman, gen, statusDone, statusFailed, statusStalled).Scan(&in, &cached)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the last foreman turn's usage of project %d: %w", project, err)
	}
	if !in.Valid && !cached.Valid {
		return false, nil // the adapter reported no usage
	}
	// The turn's whole input, cached reads included: a reported input
	// count already holds them (adapter.Usage), so cached input stands in
	// only when the input count itself is unknown.
	tokens := in.Int64
	if !in.Valid {
		tokens = cached.Int64
	}
	if tokens <= threshold {
		return false, nil
	}
	if _, err := tx.Exec("UPDATE sessions SET harness_session_id = NULL WHERE key = ?", key); err != nil {
		return false, fmt.Errorf("drop the foreman session of project %d: %w", project, err)
	}
	if _, err := tx.Exec("UPDATE projects SET foreman_session_gen = foreman_session_gen + 1 WHERE id = ?", project); err != nil {
		return false, fmt.Errorf("bump the foreman session generation of project %d: %w", project, err)
	}
	if _, err := appendEventTx(tx, evForemanSessionRotated, project, 0, 0,
		eventPayload{Tokens: tokens, Threshold: threshold, Gen: gen + 1}, now); err != nil {
		return false, err
	}
	return true, nil
}

// foremanDigestDueTx reports whether the foreman's next prompt opens a
// rotated session (§4.2): the latest rotation opened the current
// generation and no turn or chat of it has left a session id yet. Once
// one has, the session holds what the digest said, and later turns of
// the generation do not repeat it.
func foremanDigestDueTx(tx *sql.Tx, project int64) (bool, error) {
	var payload string
	err := tx.QueryRow(`SELECT payload FROM events WHERE kind = ? AND project_id = ? ORDER BY id DESC LIMIT 1`,
		evForemanSessionRotated, project).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the last foreman rotation of project %d: %w", project, err)
	}
	var p eventPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return false, nil
	}
	var gen int64
	if err := tx.QueryRow("SELECT foreman_session_gen FROM projects WHERE id = ?", project).Scan(&gen); err != nil {
		return false, fmt.Errorf("read the foreman session generation of project %d: %w", project, err)
	}
	if p.Gen != gen {
		return false, nil
	}
	var sid sql.NullString
	err = tx.QueryRow("SELECT harness_session_id FROM sessions WHERE key = ?", fmt.Sprintf("foreman:%d", project)).Scan(&sid)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read the foreman session of project %d: %w", project, err)
	}
	return !sid.Valid || sid.String == "", nil
}

// foremanPromptTx composes a foreman prompt, a headless turn's or an
// attach's opening one (§4.2): the instructions, then the wake template
// with the inbox — and, on a rotated session's first turn, the digest.
func foremanPromptTx(tx *sql.Tx, project int64, inbox ForemanInbox, now time.Time) (string, error) {
	due, err := foremanDigestDueTx(tx, project)
	if err != nil {
		return "", err
	}
	digest := ""
	if due {
		if digest, err = foremanDigestTx(tx, project, now); err != nil {
			return "", err
		}
	}
	return foreman.Instructions() + "\n" + foreman.ComposeTurnPromptDigest(digest, inbox.Events, inbox.UserMessages), nil
}

// foremanDigestTx writes the digest a fresh session starts from (§4.2):
// the plans that are active or await approval with their tasks' states,
// the needs-you queue with its reasons, and the newest history events.
// Every list is bounded and says how much it left out, and the whole is
// cut at digestMaxBytes.
func foremanDigestTx(tx *sql.Tx, project int64, now time.Time) (string, error) {
	var lines []string
	add := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	title := func(s string) string { return oneLine(s, digestLineLimit) }

	var payload string
	if err := tx.QueryRow(`SELECT payload FROM events WHERE kind = ? AND project_id = ? ORDER BY id DESC LIMIT 1`,
		evForemanSessionRotated, project).Scan(&payload); err == nil {
		var p eventPayload
		if json.Unmarshal([]byte(payload), &p) == nil {
			add("The previous session was rotated: its last turn read %d input tokens, over foreman_rotate_tokens %d.", p.Tokens, p.Threshold)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read the last foreman rotation of project %d: %w", project, err)
	}

	type plan struct {
		id           int64
		title, state string
	}
	rows, err := tx.Query(`SELECT id, title, state FROM plans WHERE project_id = ? AND state IN ('active', 'reviewing', 'pending_approval') ORDER BY id`, project)
	if err != nil {
		return "", fmt.Errorf("list the digest plans of project %d: %w", project, err)
	}
	var plans []plan
	for rows.Next() {
		var p plan
		if err := rows.Scan(&p.id, &p.title, &p.state); err != nil {
			rows.Close()
			return "", fmt.Errorf("list the digest plans of project %d: %w", project, err)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("list the digest plans of project %d: %w", project, err)
	}
	rows.Close()
	add("")
	add("Plans active, reviewing or awaiting approval: %d", len(plans))
	for i, p := range plans {
		if i == digestMaxPlans {
			add("- %d more plans omitted", len(plans)-digestMaxPlans)
			break
		}
		add("- plan %d %q: %s", p.id, title(p.title), p.state)
		tasks, err := statusPlanTasksTx(tx, p.id)
		if err != nil {
			return "", err
		}
		for j, t := range tasks {
			if j == digestMaxTasks {
				add("  - %d more tasks omitted", len(tasks)-digestMaxTasks)
				break
			}
			add("  - task %d %q: %s", t.ID, title(t.Title), t.State)
		}
	}

	needs, err := statusNeedsYouTx(tx, project, now)
	if err != nil {
		return "", err
	}
	add("")
	add("Needs you: %d", len(needs))
	for i, n := range needs {
		if i == digestMaxNeedsYou {
			add("- %d more entries omitted", len(needs)-digestMaxNeedsYou)
			break
		}
		line := fmt.Sprintf("- %s %d %q: %s", n.Kind, n.ID, title(n.Title), n.State)
		if n.Reason != "" {
			line += ": " + title(n.Reason)
		}
		add("%s", line)
	}

	erows, err := tx.Query(`SELECT kind, COALESCE(task_id, 0), payload, created_at FROM events
		WHERE project_id = ? ORDER BY id DESC LIMIT ?`, project, digestHistoryItems)
	if err != nil {
		return "", fmt.Errorf("read the digest history of project %d: %w", project, err)
	}
	var recent []string
	for erows.Next() {
		var kind, payload, at string
		var task int64
		if err := erows.Scan(&kind, &task, &payload, &at); err != nil {
			erows.Close()
			return "", fmt.Errorf("read the digest history of project %d: %w", project, err)
		}
		line := at + " " + kind
		if task != 0 {
			line += fmt.Sprintf(" task %d", task)
		}
		if text := wakeEventText(kind, payload); text != kind {
			line += ": " + text
		}
		recent = append(recent, "- "+oneLine(line, digestLineLimit))
	}
	if err := erows.Err(); err != nil {
		erows.Close()
		return "", fmt.Errorf("read the digest history of project %d: %w", project, err)
	}
	erows.Close()
	add("")
	add("Latest history, newest last (%d events):", len(recent))
	for i := len(recent) - 1; i >= 0; i-- {
		add("%s", recent[i])
	}
	return boundDigest(lines), nil
}

// boundDigest joins the digest's lines, keeping whole lines up to
// digestMaxBytes and saying how many it cut.
func boundDigest(lines []string) string {
	var b strings.Builder
	for i, l := range lines {
		if b.Len()+len(l)+1 > digestMaxBytes-80 {
			fmt.Fprintf(&b, "(%d more lines omitted; call status and history)\n", len(lines)-i)
			break
		}
		b.WriteString(l)
		b.WriteString("\n")
	}
	return b.String()
}
