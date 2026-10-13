package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/caps"
	"github.com/Blackfire-AI/brigada/internal/store"
)

// Kind is the dispatch class of a job (design §11): the three worker
// kinds, which belong to a task, and the foreman turn, which belongs to
// a project alone.
type Kind string

const (
	KindWork    Kind = "work"
	KindFix     Kind = "fix"
	KindReview  Kind = "review"
	KindForeman Kind = "foreman"
)

// Mode is how the runner starts the job (§14): a fresh session, or a
// resume of the key's session with its context kept.
type Mode string

const (
	ModeFresh   Mode = "fresh"
	ModeSession Mode = "session"
)

// Job statuses (§11). Queued, leased and running are live: the job's
// key is taken. Done, failed, cancelled and stalled are terminal: the
// key is free again.
const (
	statusQueued    = "queued"
	statusLeased    = "leased"
	statusRunning   = "running"
	statusDone      = "done"
	statusFailed    = "failed"
	statusCancelled = "cancelled"
	statusStalled   = "stalled"
)

func isLive(status string) bool {
	return status == statusQueued || status == statusLeased || status == statusRunning
}

func isTerminal(status string) bool {
	switch status {
	case statusDone, statusFailed, statusCancelled, statusStalled:
		return true
	}
	return false
}

// The runner's stop outcomes (§5.6): the outcome a completion carries
// when the runner ended the run itself — the worker stalled, or the
// run passed its class time cap. Both end the job as stalled; the
// result block tells the two apart, and the foreman that always takes
// a stalled job (§5.6) reads it there.
const (
	OutcomeStalled = "stalled"
	OutcomeTimeCap = "time_cap"
)

// isStopOutcome reports whether outcome is one the runner may complete
// a run with (§5.6).
func isStopOutcome(outcome string) bool {
	return outcome == OutcomeStalled || outcome == OutcomeTimeCap
}

// Answer is how an engine call went. Errors are reserved for the store
// and for caller mistakes (a malformed complete); a runner's protocol
// outcome — lost, gone, fenced out — is an answer, so the caller can
// branch on it instead of parsing error text.
type Answer string

const (
	AnsOK         Answer = "ok"
	AnsAlready    Answer = "already"     // the idempotent repeat of a call that took effect
	AnsCreated    Answer = "created"     // submit: a queued job now exists
	AnsPending    Answer = "pending"     // submit: a live job holds the key; the follow-up flag is set
	AnsCapped     Answer = "capped"      // submit: the task's round cap is spent; nothing was created
	AnsCancelled  Answer = "cancelled"   // the job was cancelled
	AnsGone       Answer = "gone"        // no such job, or it ended without the caller's part in it
	AnsLost       Answer = "lost"        // the lease belongs to someone else now
	AnsStaleEpoch Answer = "stale_epoch" // fenced out by a recover (§6.4)
)

// RoundCaps are the per-task caps on fix, review and restart rounds
// (§14): what the run cap of the earlier implementation became.
type RoundCaps struct {
	Fix     int
	Review  int
	Restart int
}

// Config tunes the engine. The zero value means "the default for this
// field": New fills those in, so Config{Now: clock} alone is a fully
// configured engine on an injected clock.
type Config struct {
	// Now is the engine's clock; tests inject one so they never sleep.
	Now func() time.Time
	// LeaseTTL is how long a lease lasts without a heartbeat (§5.1).
	LeaseTTL time.Duration
	// ReleaseExclude is how long a releasing runner doesn't see the job
	// it gave back: other runners go first, and a lone runner may retry
	// once the exclusion lapses.
	ReleaseExclude time.Duration
	// MaxAttempts is how many leases a job may lose before it fails as
	// "runner lost N times" instead of being offered again.
	MaxAttempts int
	// QueueTTL is how long a queued job waits for a lease before it
	// fails, so it cannot wedge its key forever.
	QueueTTL time.Duration
	// HardCaps is the wall-time cap per job kind, measured from the
	// job's first lease (§14). Entries here override the defaults for
	// their kind only.
	HardCaps map[Kind]time.Duration
	// RoundCaps are the default per-task round caps; a task's caps JSON
	// ("fix", "review", "restart") overrides them per task.
	RoundCaps RoundCaps
	// Caps are the layered concurrency caps and cost weights (§8.1),
	// as the config named them; the zero value caps nothing. A runner's
	// own advertised cap folds into the runner layer.
	Caps caps.Limits
	// DataDir is where the engine keeps its artifact blobs (§9), laid
	// out content-addressed under artifacts/<sha256[:2]>/<sha256>. Zero
	// derives the directory of the store file; a store with no file of
	// its own keeps zero, and artifact uploads answer that plainly.
	DataDir string
	// MaxArtifactBytes caps one artifact upload (§9); zero keeps the
	// 64 MiB default.
	MaxArtifactBytes int64
	// Retention is how long messages and artifacts are kept before the
	// sweep prunes them (§9); zero — the default — keeps everything.
	// The event log is never pruned.
	Retention time.Duration
	// NewLeaseID mints lease ids: <owner>-<random hex>.
	NewLeaseID func(owner int64) (string, error)
	// NewLeaseToken mints the secret of a lease-scoped foreman token
	// (§12.1): random hex, returned once in the grant and stored only
	// as its sha256.
	NewLeaseToken func() (string, error)
}

// DefaultConfig returns the shipped settings (design §14 and §5.1): a
// 90 s lease, a 120 s release exclusion, three lost attempts, a 10 m
// queue TTL, hard caps of 45 m for work and fix, 25 m for review and
// 30 m for a foreman turn, round caps of fix 3, review 3, restart 2,
// and artifact uploads capped at 64 MiB each. Retention keeps
// everything (§9); DataDir follows the store file.
func DefaultConfig() Config {
	return Config{
		Now:            func() time.Time { return time.Now().UTC() },
		LeaseTTL:       90 * time.Second,
		ReleaseExclude: 120 * time.Second,
		MaxAttempts:    3,
		QueueTTL:       10 * time.Minute,
		HardCaps: map[Kind]time.Duration{
			KindWork:    45 * time.Minute,
			KindFix:     45 * time.Minute,
			KindReview:  25 * time.Minute,
			KindForeman: 30 * time.Minute,
		},
		RoundCaps:        RoundCaps{Fix: 3, Review: 3, Restart: 2},
		MaxArtifactBytes: defaultMaxArtifactBytes,
		NewLeaseID:       newLeaseID,
		NewLeaseToken:    newLeaseToken,
	}
}

// defaultMaxArtifactBytes is the artifact upload cap (§9): room for a
// long session's transcript or check log, not for a runaway stream.
const defaultMaxArtifactBytes = 64 << 20

// newLeaseID mints the default lease id: the owning runner and random
// hex, unique per lease for the life of the store.
func newLeaseID(owner int64) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint lease id: %w", err)
	}
	return strconv.FormatInt(owner, 10) + "-" + hex.EncodeToString(b[:]), nil
}

// Engine is the lease engine (design §12.3, §14, §6.4): the rules of
// the job queue — one live job per key, leases with fencing, coalescing
// submits, and the caps that keep a lost runner from wedging a key.
// Every operation is one transaction on the store and begins with a
// sweep, so expired leases and overspent caps are dealt with before
// anything reads or writes a job.
type Engine struct {
	store *store.Store
	cfg   Config
}

// New returns an engine over s. Zero fields of cfg keep their defaults.
func New(s *store.Store, cfg Config) *Engine {
	def := DefaultConfig()
	if cfg.Now != nil {
		def.Now = cfg.Now
	}
	if cfg.LeaseTTL > 0 {
		def.LeaseTTL = cfg.LeaseTTL
	}
	if cfg.ReleaseExclude > 0 {
		def.ReleaseExclude = cfg.ReleaseExclude
	}
	if cfg.MaxAttempts > 0 {
		def.MaxAttempts = cfg.MaxAttempts
	}
	if cfg.QueueTTL > 0 {
		def.QueueTTL = cfg.QueueTTL
	}
	for kind, limit := range cfg.HardCaps {
		if limit > 0 {
			def.HardCaps[kind] = limit
		}
	}
	if cfg.RoundCaps.Fix > 0 {
		def.RoundCaps.Fix = cfg.RoundCaps.Fix
	}
	if cfg.RoundCaps.Review > 0 {
		def.RoundCaps.Review = cfg.RoundCaps.Review
	}
	if cfg.RoundCaps.Restart > 0 {
		def.RoundCaps.Restart = cfg.RoundCaps.Restart
	}
	// The caps have no nonzero default to keep: the zero Limits caps
	// nothing, which is the default, so the configured value stands as
	// given — a set limit of 0 caps that layer at nothing (§8.1).
	def.Caps = cfg.Caps
	if cfg.DataDir != "" {
		def.DataDir = cfg.DataDir
	} else if p := s.Path(); p != "" && p != ":memory:" {
		def.DataDir = filepath.Dir(p)
	}
	if cfg.MaxArtifactBytes > 0 {
		def.MaxArtifactBytes = cfg.MaxArtifactBytes
	}
	if cfg.Retention > 0 {
		def.Retention = cfg.Retention
	}
	if cfg.NewLeaseID != nil {
		def.NewLeaseID = cfg.NewLeaseID
	}
	if cfg.NewLeaseToken != nil {
		def.NewLeaseToken = cfg.NewLeaseToken
	}
	return &Engine{store: s, cfg: def}
}

// MaxArtifactBytes is the configured cap on one artifact upload.
func (e *Engine) MaxArtifactBytes() int64 { return e.cfg.MaxArtifactBytes }

// timeLayout is the store's timestamp format: fixed-width UTC text, so
// lexicographic order is chronological order and the schema's TEXT
// columns can be compared and indexed as they are written (§11).
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func fmtTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

// capText renders a cap for the error that names it: "45m", not "45m0s".
func capText(d time.Duration) string {
	return strings.TrimSuffix(d.String(), "0s")
}

// capError is the failure a hard cap writes, naming the cap (§14).
func capError(kind Kind, limit time.Duration) string {
	return fmt.Sprintf("hard cap exceeded: %s jobs are capped at %s", kind, capText(limit))
}

// boundNote keeps a release reason to 500 bytes. The cut backs off to a
// complete rune: it may not land inside one, nor on a lead byte whose
// continuation was cut off.
func boundNote(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 {
		_, size := utf8.DecodeLastRuneInString(s)
		if size == 1 && s[len(s)-1] >= utf8.RuneSelf {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}
