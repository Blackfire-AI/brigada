package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/defaults"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// Task skills (design §5.10) and the snapshot a task records of what its
// prompts carry (§5.9 'Versioning', §5.10 'Resolved once'). The project's
// skills are the last import of its skills directory, text the client
// read, never a path. A task copies the effective worker and reviewer
// texts and its ordered skills once, at creation, so a later change to a
// setting or a re-import never alters a created task.

// The bounds of §5.9 and §5.10, in bytes of UTF-8: one skill, and what a
// worker and a reviewer prompt may carry of instructions plus skills (one
// role text and two skills at their maxima for a worker, two role texts
// and two skills for a reviewer). Above a bound the import or the task is
// refused, never truncated.
const (
	MaxSkillBytes          = 4096
	MaxWorkerPromptBytes   = 16384
	MaxReviewerPromptBytes = 24576
)

// maxSkillDescriptionBytes and maxSkillTreeDigestBytes bound the import's
// smaller fields: a description is a line the foreman reads (the Agent
// Skills format caps it at 1024 characters), a tree digest a hash.
const (
	maxSkillDescriptionBytes = 1024
	maxSkillTreeDigestBytes  = 128
)

// The audiences a skill's 'for:' frontmatter names (§5.10): a worker
// skill is omitted from the reviewer's text, a reviewer skill from the
// worker prompt. Both is the default.
const (
	SkillForWorker   = "worker"
	SkillForReviewer = "reviewer"
	SkillForBoth     = "both"
)

// skillName is the Agent Skills name: lowercase letters, digits and
// hyphens, 64 at most.
var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ErrTaskInstructions reports a task whose skills or prompt bounds the
// resolver refuses: an unknown or repeated skill, or instructions plus
// skills over a prompt's bound. The whole call that named the task is
// refused.
var ErrTaskInstructions = errors.New("task instructions refused")

// Skill is one of the project's imported skills.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// For is the frontmatter's 'for:' — worker, reviewer or both; empty
	// on an import means both.
	For        string `json:"for,omitempty"`
	Text       string `json:"text"`
	Digest     string `json:"digest,omitempty"`
	TreeDigest string `json:"tree_digest,omitempty"`
}

// validateSkill checks one skill of an import and fills its defaults: a
// skill name, a known audience, a text of valid UTF-8 with no NUL and at
// most MaxSkillBytes, and a digest that matches the text when the client
// sent one. The refusal names the skill, and for the bound the size and
// the limit.
func validateSkill(s *Skill) error {
	if !skillName.MatchString(s.Name) {
		return fmt.Errorf("%w: skill name %q must be lowercase letters, digits and hyphens, at most 64", ErrInvalidSetting, s.Name)
	}
	switch s.For {
	case "":
		s.For = SkillForBoth
	case SkillForWorker, SkillForReviewer, SkillForBoth:
	default:
		return fmt.Errorf("%w: skill %s: for %q must be worker, reviewer or both", ErrInvalidSetting, s.Name, s.For)
	}
	if len(s.Text) > MaxSkillBytes {
		return fmt.Errorf("%w: skill %s is %d bytes, over the %d byte limit; shorten it (nothing is truncated)",
			ErrInvalidSetting, s.Name, len(s.Text), MaxSkillBytes)
	}
	for field, v := range map[string]string{"text": s.Text, "description": s.Description, "tree digest": s.TreeDigest} {
		if !utf8.ValidString(v) {
			return fmt.Errorf("%w: skill %s: the %s is not valid UTF-8", ErrInvalidSetting, s.Name, field)
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("%w: skill %s: the %s holds a NUL byte", ErrInvalidSetting, s.Name, field)
		}
	}
	if s.Text == "" {
		return fmt.Errorf("%w: skill %s has no text", ErrInvalidSetting, s.Name)
	}
	if len(s.Description) > maxSkillDescriptionBytes {
		return fmt.Errorf("%w: skill %s: the description is %d bytes, over the %d byte limit",
			ErrInvalidSetting, s.Name, len(s.Description), maxSkillDescriptionBytes)
	}
	if len(s.TreeDigest) > maxSkillTreeDigestBytes {
		return fmt.Errorf("%w: skill %s: the tree digest is %d bytes, over the %d byte limit",
			ErrInvalidSetting, s.Name, len(s.TreeDigest), maxSkillTreeDigestBytes)
	}
	digest := instructionsDigest(s.Text)
	if s.Digest != "" && !strings.EqualFold(s.Digest, digest) {
		return fmt.Errorf("%w: skill %s: digest %s does not match the text's %s", ErrInvalidSetting, s.Name, s.Digest, digest)
	}
	s.Digest = digest
	return nil
}

// ImportSkills replaces the repo's project's skills with an import of its
// skills directory (§5.10): every SKILL.md as text, with its name,
// description, digest and tree digest, read and sent by the client. A
// skill absent from the import is gone after it. One refused skill —
// over the 4096 byte bound, not UTF-8, a NUL, a bad name, a name given
// twice — refuses the import whole, truncates nothing and leaves the
// previous skills in effect. A re-import changes future resolutions
// only, never a created task.
func (e *Engine) ImportSkills(ctx context.Context, repoURL string, skills []Skill) ([]Skill, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	if _, cerr := Canonicalize(repoURL); cerr != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSetting, cerr)
	}
	in := make([]Skill, len(skills))
	copy(in, skills)
	seen := map[string]bool{}
	names := make([]string, 0, len(in))
	for i := range in {
		if err := validateSkill(&in[i]); err != nil {
			return nil, err
		}
		if seen[in[i].Name] {
			return nil, fmt.Errorf("%w: skill %s is given twice", ErrInvalidSetting, in[i].Name)
		}
		seen[in[i].Name] = true
		names = append(names, in[i].Name)
	}
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			project, err = createProjectTx(tx, repoURL, setupDefaultBranch, "", "", now)
			if err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		if err := activeProjectTx(tx, project); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM skills WHERE project_id = ?", project); err != nil {
			return fmt.Errorf("replace the skills of project %d: %w", project, err)
		}
		for _, s := range in {
			if _, err := tx.Exec(`INSERT INTO skills (project_id, name, description, for_role, text, digest, tree_digest)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, project, s.Name, s.Description, s.For, s.Text, s.Digest, s.TreeDigest); err != nil {
				return fmt.Errorf("import skill %s of project %d: %w", s.Name, project, err)
			}
		}
		reason := "skills: none (imported)"
		if len(names) > 0 {
			reason = fmt.Sprintf("skills: %s (imported)", strings.Join(names, ","))
		}
		_, err = appendEventTx(tx, evProjectSettingSet, project, 0, 0, eventPayload{Reason: reason}, now)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrInvalidSetting) {
			return nil, err
		}
		return nil, fmt.Errorf("import the skills of the project of %s: %w", RedactRepoURL(repoURL), err)
	}
	return e.Skills(ctx, repoURL)
}

// Skills lists the repo's project's imported skills by name. A repo with
// no project has none.
func (e *Engine) Skills(ctx context.Context, repoURL string) ([]Skill, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("%w: repo URL is required (product §5 rule 6: repos are addressed by URL)", ErrInvalidSetting)
	}
	var out []Skill
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		var project int64
		err := lookupProjectTx(tx, repoURL, "id", &project)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the project of %s: %w", RedactRepoURL(repoURL), err)
		}
		all, err := projectSkillsTx(tx, project)
		if err != nil {
			return err
		}
		for _, s := range all {
			out = append(out, s)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortSkills(out)
	return out, nil
}

// projectSkillsTx reads a project's imported skills, keyed by name.
func projectSkillsTx(tx *sql.Tx, project int64) (map[string]Skill, error) {
	rows, err := tx.Query(`SELECT name, description, for_role, text, digest, tree_digest FROM skills WHERE project_id = ?`, project)
	if err != nil {
		return nil, fmt.Errorf("read the skills of project %d: %w", project, err)
	}
	defer rows.Close()
	out := map[string]Skill{}
	for rows.Next() {
		var s Skill
		if err := rows.Scan(&s.Name, &s.Description, &s.For, &s.Text, &s.Digest, &s.TreeDigest); err != nil {
			return nil, fmt.Errorf("read the skills of project %d: %w", project, err)
		}
		out[s.Name] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the skills of project %d: %w", project, err)
	}
	return out, nil
}

func sortSkills(s []Skill) {
	slices.SortFunc(s, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
}

// RoleSnapshot is a role text as a task recorded it: the text and its
// provenance — source, settings version, digest and size.
type RoleSnapshot struct {
	Text    string `json:"text,omitempty"`
	Source  string `json:"source,omitempty"`
	Version string `json:"version,omitempty"`
	Digest  string `json:"digest,omitempty"`
	Bytes   int    `json:"bytes"`
}

// TaskSkill is one skill as a task recorded it, in the order given.
type TaskSkill struct {
	Name       string `json:"name"`
	For        string `json:"for"`
	Text       string `json:"text"`
	Digest     string `json:"digest"`
	TreeDigest string `json:"tree_digest,omitempty"`
}

// TaskInstructions is the snapshot a task takes at creation of what its
// prompts carry: the worker and reviewer texts and the ordered skills.
type TaskInstructions struct {
	Worker   RoleSnapshot `json:"worker"`
	Reviewer RoleSnapshot `json:"reviewer"`
	Skills   []TaskSkill  `json:"skills,omitempty"`
}

// WorkerBytes is what the worker prompt carries of instructions plus
// skills: the worker text and every skill not meant for the reviewer
// alone.
func (ti TaskInstructions) WorkerBytes() int {
	n := len(ti.Worker.Text)
	for _, s := range ti.Skills {
		if s.For != SkillForReviewer {
			n += len(s.Text)
		}
	}
	return n
}

// ReviewerBytes is what the reviewer prompt carries of instructions plus
// skills: the reviewer text, the worker text as the convention the
// author followed, and every skill not meant for the worker alone.
func (ti TaskInstructions) ReviewerBytes() int {
	n := len(ti.Reviewer.Text) + len(ti.Worker.Text)
	for _, s := range ti.Skills {
		if s.For != SkillForWorker {
			n += len(s.Text)
		}
	}
	return n
}

// effectiveRoleTx reads one role's effective instruction text for a
// task's snapshot (§5.9), inside the resolver's transaction: the stored
// role row, or the live embedded default when the project has none. The
// same read RoleInstructions makes for its caller.
func effectiveRoleTx(tx *sql.Tx, project int64, role string) (*RoleInstructions, error) {
	var text, rawMeta string
	err := tx.QueryRow("SELECT text, meta FROM role_instructions WHERE project_id = ? AND role = ?", project, role).Scan(&text, &rawMeta)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not configured: no row, so the live embedded default
		// applies (§5.9).
		live, err := defaults.LiveRoleText(role)
		if err != nil {
			return nil, err
		}
		return &RoleInstructions{Role: role, State: RoleTextNotConfigured, Text: live, Source: SourceDefault,
			Version: defaults.Version, Bytes: len(live), Digest: instructionsDigest(live)}, nil
	case err != nil:
		return nil, fmt.Errorf("read the %s instructions of project %d: %w", role, project, err)
	}
	var meta roleTextMeta
	if rawMeta != "" {
		if err := json.Unmarshal([]byte(rawMeta), &meta); err != nil {
			return nil, fmt.Errorf("parse the %s instructions of project %d: %w", role, project, err)
		}
	}
	out := &RoleInstructions{Role: role, Text: text, Source: meta.Source, Version: meta.Version, Bytes: meta.Bytes, Digest: meta.Digest}
	switch {
	case strings.HasPrefix(meta.Source, store.RoleSourceSeededPrefix),
		meta.Source == "" && text == "":
		// The upgrade's seed is no user value: the role is not
		// configured, and the seeded default stays in effect.
		out.State = RoleTextNotConfigured
	case text == "":
		out.State = RoleTextEmptyByChoice
	default:
		out.State = RoleTextFile
	}
	return out, nil
}

// roleSnapshotOf records a role's effective text as a task's copy of it.
func roleSnapshotOf(ri *RoleInstructions) RoleSnapshot {
	return RoleSnapshot{Text: ri.Text, Source: ri.Source, Version: ri.Version, Digest: instructionsDigest(ri.Text), Bytes: len(ri.Text)}
}

// resolveTaskInstructionsTx resolves what a task's prompts will carry
// (§5.9 'Versioning', §5.10 'Resolved once'): the effective worker and
// reviewer role texts — the role's row, or the live default when it is
// not configured, an empty-by-choice role copied as empty — each with
// its provenance, and the named skills copied in the order given. An
// unknown skill or one named twice refuses the task whole; so does a
// worker prompt whose instructions plus skills pass
// MaxWorkerPromptBytes or a reviewer prompt passing
// MaxReviewerPromptBytes, a 'for:' skill counting only in the prompt it
// appears in. The refusal names the task, and for a bound the size and
// the limit; nothing is truncated.
func resolveTaskInstructionsTx(tx *sql.Tx, project int64, task string, skillNames []string) (TaskInstructions, error) {
	var out TaskInstructions
	if len(skillNames) > 0 {
		have, err := projectSkillsTx(tx, project)
		if err != nil {
			return TaskInstructions{}, err
		}
		seen := map[string]bool{}
		for _, name := range skillNames {
			if seen[name] {
				return TaskInstructions{}, fmt.Errorf("%w: task %q names skill %q twice", ErrTaskInstructions, task, name)
			}
			seen[name] = true
			s, ok := have[name]
			if !ok {
				return TaskInstructions{}, fmt.Errorf("%w: task %q names skill %q, which the project has not imported; known skills: %s",
					ErrTaskInstructions, task, name, knownSkills(have))
			}
			out.Skills = append(out.Skills, TaskSkill{Name: s.Name, For: s.For, Text: s.Text, Digest: s.Digest, TreeDigest: s.TreeDigest})
		}
	}
	worker, err := effectiveRoleTx(tx, project, RoleWorker)
	if err != nil {
		return TaskInstructions{}, err
	}
	out.Worker = roleSnapshotOf(worker)
	reviewer, err := effectiveRoleTx(tx, project, RoleReviewer)
	if err != nil {
		return TaskInstructions{}, err
	}
	out.Reviewer = roleSnapshotOf(reviewer)
	if n := out.WorkerBytes(); n > MaxWorkerPromptBytes {
		return TaskInstructions{}, fmt.Errorf("%w: task %q: the worker prompt's instructions and skills are %d bytes, over the %d byte limit; pick fewer or shorter skills (nothing is truncated)",
			ErrTaskInstructions, task, n, MaxWorkerPromptBytes)
	}
	if n := out.ReviewerBytes(); n > MaxReviewerPromptBytes {
		return TaskInstructions{}, fmt.Errorf("%w: task %q: the reviewer prompt's instructions and skills are %d bytes, over the %d byte limit; pick fewer or shorter skills (nothing is truncated)",
			ErrTaskInstructions, task, n, MaxReviewerPromptBytes)
	}
	return out, nil
}

// knownSkills names a project's skills for a refusal, "none" for none.
func knownSkills(have map[string]Skill) string {
	if len(have) == 0 {
		return "none"
	}
	all := make([]Skill, 0, len(have))
	for _, s := range have {
		all = append(all, s)
	}
	sortSkills(all)
	names := make([]string, len(all))
	for i, s := range all {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

// recordTaskInstructionsTx writes a resolved snapshot onto a task: the
// worker text into the task's instructions columns, which every job of
// the task reads, with the provenance shape they have held since 0035,
// and the reviewer text and the ordered skills beside it.
func recordTaskInstructionsTx(tx *sql.Tx, taskID int64, ti TaskInstructions, now time.Time) error {
	wm, err := json.Marshal(instructionsMeta{Source: ti.Worker.Source, Hash: ti.Worker.Version, Digest: ti.Worker.Digest, Bytes: ti.Worker.Bytes})
	if err != nil {
		return fmt.Errorf("encode the worker instructions of task %d: %w", taskID, err)
	}
	rm, err := json.Marshal(roleTextMeta{Source: ti.Reviewer.Source, Version: ti.Reviewer.Version, Digest: ti.Reviewer.Digest, Bytes: ti.Reviewer.Bytes})
	if err != nil {
		return fmt.Errorf("encode the reviewer instructions of task %d: %w", taskID, err)
	}
	skills := ""
	if len(ti.Skills) > 0 {
		b, err := json.Marshal(ti.Skills)
		if err != nil {
			return fmt.Errorf("encode the skills of task %d: %w", taskID, err)
		}
		skills = string(b)
	}
	if _, err := tx.Exec(`UPDATE tasks SET instructions = ?, instructions_meta = ?, reviewer_instructions = ?,
		reviewer_instructions_meta = ?, skills = ?, updated_at = ? WHERE id = ?`,
		ti.Worker.Text, string(wm), ti.Reviewer.Text, string(rm), skills, fmtTime(now), taskID); err != nil {
		return fmt.Errorf("record the instructions of task %d: %w", taskID, err)
	}
	return nil
}

// reresolveTaskInstructionsTx re-resolves what a task that has not
// started carries (§5.9 'Versioning', §5.10): the current texts, with
// the skills the caller names — nil keeps the names the task recorded,
// any list, empty included, replaces them — and the fresh resolution
// recorded over the old one.
func reresolveTaskInstructionsTx(tx *sql.Tx, project, taskID int64, task string, skills []string, now time.Time) error {
	names := skills
	if names == nil {
		snap, err := taskSnapshotTx(tx, taskID)
		if err != nil {
			return err
		}
		names = make([]string, len(snap.Skills))
		for i, s := range snap.Skills {
			names[i] = s.Name
		}
	}
	ti, err := resolveTaskInstructionsTx(tx, project, task, names)
	if err != nil {
		return err
	}
	return recordTaskInstructionsTx(tx, taskID, ti, now)
}

// taskStartedTx reports whether a task has queued or run any job: only
// a task with none may have its snapshot re-resolved by an amendment.
func taskStartedTx(tx *sql.Tx, taskID int64) (bool, error) {
	var started bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM jobs WHERE task_id = ?)`, taskID).Scan(&started); err != nil {
		return false, fmt.Errorf("read the jobs of task %d: %w", taskID, err)
	}
	return started, nil
}

// taskSnapshotTx reads the snapshot a task recorded. A task recorded
// before the skills arrived carries its worker text with the older
// provenance, whose settings version is the import hash, an explicitly
// empty reviewer text and no skills.
func taskSnapshotTx(tx *sql.Tx, taskID int64) (TaskInstructions, error) {
	var wText, wMeta, rText, rMeta, skills string
	if err := tx.QueryRow(`SELECT instructions, instructions_meta, reviewer_instructions, reviewer_instructions_meta, skills
		FROM tasks WHERE id = ?`, taskID).Scan(&wText, &wMeta, &rText, &rMeta, &skills); err != nil {
		return TaskInstructions{}, fmt.Errorf("read the instructions of task %d: %w", taskID, err)
	}
	var out TaskInstructions
	var err error
	if out.Worker, err = parseRoleSnapshot(wText, wMeta); err != nil {
		return TaskInstructions{}, fmt.Errorf("parse the worker instructions of task %d: %w", taskID, err)
	}
	if out.Reviewer, err = parseRoleSnapshot(rText, rMeta); err != nil {
		return TaskInstructions{}, fmt.Errorf("parse the reviewer instructions of task %d: %w", taskID, err)
	}
	if skills != "" {
		if err := json.Unmarshal([]byte(skills), &out.Skills); err != nil {
			return TaskInstructions{}, fmt.Errorf("parse the skills of task %d: %w", taskID, err)
		}
	}
	return out, nil
}

// parseRoleSnapshot reads a recorded text and its provenance, either
// shape: the role meta's version, or the older instructions meta's hash.
func parseRoleSnapshot(text, raw string) (RoleSnapshot, error) {
	rs := RoleSnapshot{Text: text, Bytes: len(text)}
	if raw == "" {
		return rs, nil
	}
	var m struct {
		roleTextMeta
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return RoleSnapshot{}, err
	}
	rs.Source, rs.Version, rs.Digest = m.Source, m.Version, m.Digest
	if rs.Version == "" {
		rs.Version = m.Hash
	}
	return rs, nil
}
