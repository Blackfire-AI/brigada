package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The artifact kinds the engine stores (§9): the session transcript a
// run's events make, and the full log of a check the runner ran (§5.5).
// The set lives in the schema's CHECK; these are the words the engine
// itself writes.
const (
	ArtifactTranscript = "transcript"
	ArtifactCheckLog   = "check_log"
	// ArtifactSetupLog is the log of the setup command (§5.7), kept next
	// to the check log so a failed setup can be read.
	ArtifactSetupLog = "setup_log"
)

// ErrInvalidArtifact marks a refusal the caller caused: an artifact the
// engine will not take, whatever its reason. The API turns it into the
// caller's 400; every other upload error is the engine's own.
var ErrInvalidArtifact = errors.New("invalid artifact")

// ErrArtifactTooLarge is the upload cap speaking (§9): one blob over
// MaxArtifactBytes. The API answers it with the caller's 413.
var ErrArtifactTooLarge = errors.New("artifact over the size cap")

// ArtifactInfo is one stored artifact as the API shows it (§9): the
// row behind a transcript or check log, with the blob's content
// address.
type ArtifactInfo struct {
	ID        int64  `json:"id"`
	JobID     int64  `json:"job_id"`
	ProjectID int64  `json:"project_id"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	CreatedAt string `json:"created_at"`
}

// artifactColumns reads an artifact whole with its job's project, for
// the scoping a foreman token answers to.
const artifactColumns = `a.id, a.job_id, j.project_id, a.kind, a.path, a.size, a.sha256, a.created_at`

func scanArtifact(row interface{ Scan(dest ...any) error }) (*ArtifactInfo, error) {
	a := &ArtifactInfo{}
	err := row.Scan(&a.ID, &a.JobID, &a.ProjectID, &a.Kind, &a.Path, &a.Size, &a.SHA256, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// UploadArtifact stores one artifact blob of a run the caller holds
// (§9, §12.3): the session's transcript or the check's log. It is
// fenced exactly like a complete — the lease id must be the job's, the
// caller its holder, the fence epoch current — and accepted, like a
// complete, while a lapsed lease nobody re-leased still carries the id.
// The blob lands in a content-addressed file under the engine's data
// directory (artifacts/<sha256[:2]>/<sha256>), with the store row
// naming job, kind, path, size and digest; the same content for the
// same job and kind is idempotent — already, the first row back — and
// a transcript upload points the job's transcript ref at the row.
func (e *Engine) UploadArtifact(ctx context.Context, owner, jobID int64, leaseID, kind string, data []byte) (Answer, *ArtifactInfo, error) {
	switch kind {
	case ArtifactTranscript, ArtifactCheckLog, ArtifactSetupLog:
	default:
		return "", nil, fmt.Errorf("%w: kind must be %s, %s or %s, not %q",
			ErrInvalidArtifact, ArtifactTranscript, ArtifactCheckLog, ArtifactSetupLog, kind)
	}
	if int64(len(data)) > e.cfg.MaxArtifactBytes {
		return "", nil, fmt.Errorf("%w: %d bytes, cap %d", ErrArtifactTooLarge, len(data), e.cfg.MaxArtifactBytes)
	}
	if e.cfg.DataDir == "" {
		// A store with no file of its own (an in-memory test store, say)
		// and no configured directory: the engine's own setup is short,
		// not the caller's call.
		return "", nil, fmt.Errorf("upload %s artifact for job %d: the engine has no data directory for artifact blobs", kind, jobID)
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])

	var ans Answer
	var info *ArtifactInfo
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		now := e.cfg.Now()
		if _, err := e.sweepTx(tx, now); err != nil {
			return err
		}
		a, err := uploadArtifactTx(tx, e.cfg, owner, jobID, leaseID, kind, data, sha, now)
		if err != nil {
			return fmt.Errorf("upload %s artifact for job %d: %w", kind, jobID, err)
		}
		ans, info = a.answer, a.info
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return ans, info, nil
}

// uploadResult is what one upload answered, and the row it named.
type uploadResult struct {
	answer Answer
	info   *ArtifactInfo
}

// uploadArtifactTx is the upload inside the transaction: the fence
// checks, the idempotency look-up, the blob's file and its row.
func uploadArtifactTx(tx *sql.Tx, cfg Config, owner, jobID int64, leaseID, kind string, data []byte, sha string, now time.Time) (uploadResult, error) {
	job, err := loadJobTx(tx, jobID)
	if err != nil {
		return uploadResult{}, err
	}
	switch {
	case job == nil:
		return uploadResult{answer: AnsGone}, nil
	case job.Status == statusCancelled:
		return uploadResult{answer: AnsCancelled}, nil
	case isTerminal(job.Status):
		// A completion ended the job while the upload was in flight. The
		// blob the finished run already stored stays reachable — that is
		// the idempotent repeat — but nothing new is taken.
		if job.DoneLease == leaseID {
			if a, ok, err := artifactByShaTx(tx, jobID, kind, sha); err != nil {
				return uploadResult{}, err
			} else if ok {
				return uploadResult{answer: AnsAlready, info: a}, nil
			}
		}
		return uploadResult{answer: AnsGone}, nil
	case job.LeaseID == "" || job.LeaseID != leaseID:
		return uploadResult{answer: AnsLost}, nil
	case job.LeaseOwner != owner:
		return uploadResult{answer: AnsLost}, nil
	}
	epoch, err := epochTx(tx)
	if err != nil {
		return uploadResult{}, err
	}
	if job.LeaseEpoch < epoch {
		return uploadResult{answer: AnsStaleEpoch}, nil
	}
	if a, ok, err := artifactByShaTx(tx, jobID, kind, sha); err != nil {
		return uploadResult{}, err
	} else if ok {
		return uploadResult{answer: AnsAlready, info: a}, nil
	}

	// The blob is written before its row, and lands in one piece: a row
	// without its file must never exist, while a file without its row is
	// only an orphan a later upload of the same content adopts — content
	// addressing makes the two interchangeable.
	path, _ := blobPath(cfg.DataDir, sha)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return uploadResult{}, fmt.Errorf("make the artifact directory: %w", err)
	}
	if err := writeBlob(path, data); err != nil {
		return uploadResult{}, fmt.Errorf("write the artifact blob: %w", err)
	}
	res, err := tx.Exec(`INSERT INTO artifacts (job_id, kind, path, size, sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, jobID, kind, path, len(data), sha, fmtTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			// The same content raced us to the row; the blob it wrote is
			// the one this content addresses, so its row is the answer.
			if a, ok, aerr := artifactByShaTx(tx, jobID, kind, sha); aerr == nil && ok {
				return uploadResult{answer: AnsAlready, info: a}, nil
			}
		}
		return uploadResult{}, fmt.Errorf("store the artifact row: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return uploadResult{}, fmt.Errorf("store the artifact row: last insert id: %w", err)
	}
	if kind == ArtifactTranscript {
		// The job's transcript ref (§9): what a reader follows from the
		// job to the session's own record of itself.
		if _, err := tx.Exec("UPDATE jobs SET transcript_id = ?, updated_at = ? WHERE id = ?",
			id, fmtTime(now), jobID); err != nil {
			return uploadResult{}, fmt.Errorf("point the transcript ref: %w", err)
		}
	}
	return uploadResult{answer: AnsOK, info: &ArtifactInfo{
		ID: id, JobID: jobID, ProjectID: job.ProjectID, Kind: kind,
		Path: path, Size: int64(len(data)), SHA256: sha, CreatedAt: fmtTime(now),
	}}, nil
}

// artifactByShaTx reads the artifact row a job's kind and content
// already have, if any: the idempotency an upload answers already with.
func artifactByShaTx(tx *sql.Tx, jobID int64, kind, sha string) (*ArtifactInfo, bool, error) {
	a, err := scanArtifact(tx.QueryRow(`SELECT `+artifactColumns+`
		FROM artifacts a JOIN jobs j ON j.id = a.job_id
		WHERE a.job_id = ? AND a.kind = ? AND a.sha256 = ?`, jobID, kind, sha))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the artifact by digest: %w", err)
	}
	return a, true, nil
}

// Artifact reads one artifact's row and opens its blob for streaming
// (§9). Where the blob lives is derived from the row's digest, never
// read from the row: the path a row carries is bookkeeping, not a
// capability, so nothing that ever touched the store can point the
// engine's readers at a file of its choosing. The caller closes the
// file. nil, nil, nil when there is no such artifact — its digest is
// not a content address, or its blob is gone (pruned, lost) — which
// reads as not-found from outside, not as the engine's failure.
func (e *Engine) Artifact(ctx context.Context, id int64) (*ArtifactInfo, *os.File, error) {
	var info *ArtifactInfo
	var blob *os.File
	err := e.store.View(ctx, func(tx *sql.Tx) error {
		a, err := scanArtifact(tx.QueryRow(`SELECT `+artifactColumns+`
			FROM artifacts a JOIN jobs j ON j.id = a.job_id WHERE a.id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read artifact %d: %w", id, err)
		}
		path, ok := blobPath(e.cfg.DataDir, a.SHA256)
		if !ok {
			return nil // a digest that is not a content address names nothing
		}
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil // the blob is gone: not-found, not a fault
		}
		if err != nil {
			return fmt.Errorf("open the blob of artifact %d: %w", id, err)
		}
		a.Path = path
		info, blob = a, f
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("read artifact %d: %w", id, err)
	}
	return info, blob, nil
}

// blobPath is where a digest's blob lives (§9): 64 lowercase hex
// characters, laid out content-addressed under the data directory. ok
// is false for anything a digest is not — a row whose sha256 column is
// not a content address names no file, whatever its path column says.
func blobPath(dataDir, sha string) (string, bool) {
	if len(sha) != sha256.Size*2 { // a digest's hex, not a shorter or longer word
		return "", false
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return filepath.Join(dataDir, "artifacts", sha[:2], sha), true
}

// writeBlob puts a blob at its content address in one piece: the bytes
// reach a temp file beside the address, are flushed to the storage
// beneath it, and are renamed into it, so a reader of a live address
// never sees a half-written blob and a crash leaves at worst an orphan
// temp — never a truncated blob under the address, which the next
// upload of the same content would have to repair. A rename over an
// existing file replaces it whole; content addressing makes the two
// the same bytes.
func writeBlob(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// PruneReport counts what a prune took (§9).
type PruneReport struct {
	Messages  int64 `json:"messages"`
	Artifacts int64 `json:"artifacts"`
	Files     int64 `json:"files"`
}

// Prune applies the retention policy by hand (§9): messages and
// artifacts created before the cut go — the artifacts' blob files with
// them — and the event log keeps everything, append-only and forever
// the backbone. A blob another row still references stays: content
// addressing means one file can back many rows. The sweep also takes
// the upload temps crashed writers left behind, once older than any
// upload still in flight.
//
// The counting and the file removal run inside the prune's own
// transaction, not after it: the store serialises writers, so an
// upload of the same content cannot commit a row between the count
// that spare the file and the remove that takes it. The trade is that
// a transaction which fails after removing files rolls the rows back
// while the blobs stay gone — read as not-found (Artifact), never as
// corruption. File removal itself is best-effort; the rows are gone
// whatever it answered.
func (e *Engine) Prune(ctx context.Context, before time.Time) (PruneReport, error) {
	var rep PruneReport
	err := e.store.Update(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec("DELETE FROM messages WHERE created_at < ?", fmtTime(before))
		if err != nil {
			return fmt.Errorf("prune messages: %w", err)
		}
		if rep.Messages, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("prune messages: rows: %w", err)
		}
		rows, err := tx.Query("SELECT id, sha256 FROM artifacts WHERE created_at < ?", fmtTime(before))
		if err != nil {
			return fmt.Errorf("list pruned artifacts: %w", err)
		}
		var ids []int64
		var shas []string
		for rows.Next() {
			var id int64
			var sha string
			if err := rows.Scan(&id, &sha); err != nil {
				rows.Close()
				return fmt.Errorf("list pruned artifacts: %w", err)
			}
			ids = append(ids, id)
			if !slices.Contains(shas, sha) {
				shas = append(shas, sha)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list pruned artifacts: %w", err)
		}
		rows.Close()
		rep.Artifacts = int64(len(ids))
		for _, id := range ids {
			if _, err := tx.Exec("DELETE FROM artifacts WHERE id = ?", id); err != nil {
				return fmt.Errorf("prune artifact %d: %w", id, err)
			}
		}
		for _, sha := range shas {
			var left int
			if err := tx.QueryRow("SELECT COUNT(*) FROM artifacts WHERE sha256 = ?", sha).Scan(&left); err != nil {
				return fmt.Errorf("count references of %s: %w", sha, err)
			}
			if left > 0 {
				continue // a younger row still addresses this blob
			}
			path, ok := blobPath(e.cfg.DataDir, sha)
			if !ok {
				continue // not a content address: no file of ours to take
			}
			if os.Remove(path) == nil {
				rep.Files++
			}
		}
		return nil
	})
	if err != nil {
		return PruneReport{}, fmt.Errorf("prune before %s: %w", fmtTime(before), err)
	}
	rep.Files += int64(sweepStaleTemps(filepath.Join(e.cfg.DataDir, "artifacts"), e.cfg.Now()))
	return rep, nil
}

// staleTempAge is how old an orphaned upload temp must be before the
// prune takes it: a younger one may belong to an upload in flight
// right now, and its writer renames it away on its own.
const staleTempAge = time.Hour

// sweepStaleTemps removes the upload temps crashed writers left behind
// (writeBlob): only an hour-old orphan goes, never a live upload's.
// Best-effort, like every file removal the prune makes.
func sweepStaleTemps(root string, now time.Time) int {
	var taken int
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), ".blob-") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if now.Sub(info.ModTime()) > staleTempAge && os.Remove(path) == nil {
			taken++
		}
		return nil
	})
	return taken
}
