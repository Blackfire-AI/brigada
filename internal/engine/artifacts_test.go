package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newArtifactEnv is an env whose engine has a data directory for
// artifact blobs, plus where that directory is.
func newArtifactEnv(t *testing.T, adjust func(*Config)) (*env, string) {
	t.Helper()
	dir := t.TempDir()
	var dataDir string
	e := newEnvCfg(t, func(c *Config) {
		c.DataDir = dir
		dataDir = dir
		if adjust != nil {
			adjust(c)
		}
	})
	return e, dataDir
}

// upload is the env's artifact upload, failing on the engine's own
// errors and handing back the protocol answer with the row.
func (e *env) upload(owner, jobID int64, leaseID, kind string, data []byte) (Answer, *ArtifactInfo) {
	e.t.Helper()
	ans, info, err := e.eng.UploadArtifact(e.ctx, owner, jobID, leaseID, kind, data)
	if err != nil {
		e.t.Fatalf("upload %s artifact for job %d: %v", kind, jobID, err)
	}
	return ans, info
}

func TestUploadArtifactStoresContentAddressed(t *testing.T) {
	e, dataDir := newArtifactEnv(t, nil)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	transcript := []byte("one raw event line\nanother raw event line\n")
	sum := sha256.Sum256(transcript)
	sha := hex.EncodeToString(sum[:])

	ans, info := e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, transcript)
	if ans != AnsOK || info == nil {
		t.Fatalf("transcript upload = %s %+v, want ok with the row back", ans, info)
	}
	wantPath := filepath.Join(dataDir, "artifacts", sha[:2], sha)
	if info.Path != wantPath {
		t.Errorf("artifact path = %q, want the content-addressed %q", info.Path, wantPath)
	}
	if info.Size != int64(len(transcript)) || info.SHA256 != sha {
		t.Errorf("artifact row = size %d sha %s, want %d and %s", info.Size, info.SHA256, len(transcript), sha)
	}
	if got, err := os.ReadFile(wantPath); err != nil || string(got) != string(transcript) {
		t.Errorf("the blob on disk = %q (%v), want the uploaded bytes", got, err)
	}

	// The job's transcript ref points at its transcript artifact (§9).
	if got := e.int("SELECT COALESCE(transcript_id, 0) FROM jobs WHERE id = ?", g.JobID); got != info.ID {
		t.Errorf("job transcript ref = %d, want the artifact %d", got, info.ID)
	}

	// A check log is a second artifact of the same job, its own content.
	ans, log := e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactCheckLog, []byte("check output"))
	if ans != AnsOK {
		t.Fatalf("check log upload = %s, want ok", ans)
	}
	if log.ID == info.ID {
		t.Error("the check log reused the transcript's row")
	}
	if n := e.int("SELECT COUNT(*) FROM artifacts WHERE job_id = ?", g.JobID); n != 2 {
		t.Errorf("artifacts = %d, want 2", n)
	}
}

// TestUploadArtifactReplacesTheBlobWhole pins how a blob lands at its
// content address: in one piece. The bytes reach a temp file beside the
// address and rename into it, so whatever sat at the address before — a
// truncated leftover from a crash mid-write — is replaced whole rather
// than appended to or patched, and the directory is left holding the
// blob and nothing else: no temp beside it, no partial file under the
// address.
func TestUploadArtifactReplacesTheBlobWhole(t *testing.T) {
	e, dataDir := newArtifactEnv(t, nil)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	blob := bytes.Repeat([]byte("one whole piece. "), 64)
	sum := sha256.Sum256(blob)
	sha := hex.EncodeToString(sum[:])
	path := filepath.Join(dataDir, "artifacts", sha[:2], sha)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make the artifact directory: %v", err)
	}
	if err := os.WriteFile(path, blob[:len(blob)/2], 0o644); err != nil {
		t.Fatalf("write the truncated leftover: %v", err)
	}

	if ans, _ := e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, blob); ans != AnsOK {
		t.Fatalf("upload over the leftover = %s, want ok", ans)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, blob) {
		t.Errorf("the blob at the address = %d bytes (%v), want the %d uploaded bytes whole", len(got), err, len(blob))
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read the artifact directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != sha {
		names := make([]string, len(entries))
		for i, en := range entries {
			names[i] = en.Name()
		}
		t.Errorf("the artifact directory holds %v, want only the blob %s: nothing beside it", names, sha)
	}
}

func TestUploadArtifactIsIdempotentByDigest(t *testing.T) {
	e, _ := newArtifactEnv(t, nil)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	data := []byte("the same bytes twice")
	_, first := e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, data)
	ans, second := e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, data)
	if ans != AnsAlready {
		t.Fatalf("the repeat upload = %s, want already", ans)
	}
	if second.ID != first.ID {
		t.Errorf("the repeat answered artifact %d, want the first row %d", second.ID, first.ID)
	}
	if n := e.int("SELECT COUNT(*) FROM artifacts WHERE job_id = ?", g.JobID); n != 1 {
		t.Errorf("artifacts = %d, want the one row the first upload wrote", n)
	}
}

func TestUploadArtifactEnforcesTheCap(t *testing.T) {
	e, dataDir := newArtifactEnv(t, func(c *Config) { c.MaxArtifactBytes = 16 })
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	big := make([]byte, 17)
	ans, info, err := e.eng.UploadArtifact(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, big)
	if ans != "" || info != nil || err == nil {
		t.Fatalf("over-cap upload = %s %+v %v, want a refusal", ans, info, err)
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("the refusal = %v, want it to name the cap", err)
	}
	if n := e.int("SELECT COUNT(*) FROM artifacts"); n != 0 {
		t.Errorf("artifacts = %d, want none", n)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "artifacts")); !os.IsNotExist(err) {
		t.Errorf("the refused upload left files behind: %v", err)
	}

	// The cap is inclusive: the boundary itself passes.
	if ans, _ := e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, make([]byte, 16)); ans != AnsOK {
		t.Errorf("the boundary size upload = %s, want ok", ans)
	}
}

func TestUploadArtifactIsFenced(t *testing.T) {
	e, _ := newArtifactEnv(t, nil)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)

	if ans, _ := e.upload(g.LeaseOwner, g.JobID, "not-the-lease", ArtifactTranscript, []byte("x")); ans != AnsLost {
		t.Errorf("upload with a wrong lease = %s, want lost", ans)
	}
	if ans, _ := e.upload(e.runner2, g.JobID, g.LeaseID, ArtifactTranscript, []byte("x")); ans != AnsLost {
		t.Errorf("upload from a non-holder = %s, want lost", ans)
	}
	if ans, _ := e.upload(g.LeaseOwner, 999, g.LeaseID, ArtifactTranscript, []byte("x")); ans != AnsGone {
		t.Errorf("upload for a missing job = %s, want gone", ans)
	}

	// A recover fenced the caller out (§6.4).
	if _, err := e.eng.BumpFenceEpoch(e.ctx); err != nil {
		t.Fatalf("bump the fence epoch: %v", err)
	}
	if ans, _ := e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, []byte("x")); ans != AnsStaleEpoch {
		t.Errorf("upload past a fence epoch = %s, want stale_epoch", ans)
	}

	// A cancelled job takes no artifacts.
	e2, _ := newArtifactEnv(t, nil)
	r := e2.submit(e2.work())
	e2.cancel(r.JobID)
	if ans, _ := e2.upload(e2.runner1, r.JobID, "any", ArtifactTranscript, []byte("x")); ans != AnsCancelled {
		t.Errorf("upload for a cancelled job = %s, want cancelled", ans)
	}

	// A job a completion finished answers with what that run already
	// stored, never by taking new bytes.
	e3, _ := newArtifactEnv(t, nil)
	e3.submit(e3.work())
	g3 := e3.leaseAny(e3.runner1)
	stored := []byte("the finished run's transcript")
	e3.upload(g3.LeaseOwner, g3.JobID, g3.LeaseID, ArtifactTranscript, stored)
	e3.complete(g3, done())
	ans, again := e3.upload(g3.LeaseOwner, g3.JobID, g3.LeaseID, ArtifactTranscript, stored)
	if ans != AnsAlready || again == nil {
		t.Fatalf("upload after the completion = %s %+v, want already with the stored row", ans, again)
	}
	if ans, _ := e3.upload(g3.LeaseOwner, g3.JobID, g3.LeaseID, ArtifactTranscript, []byte("new bytes")); ans != AnsGone {
		t.Errorf("upload of new bytes after the completion = %s, want gone", ans)
	}
}

func TestUploadArtifactRejectsUnknownKinds(t *testing.T) {
	e, _ := newArtifactEnv(t, nil)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	if _, _, err := e.eng.UploadArtifact(e.ctx, g.LeaseOwner, g.JobID, g.LeaseID, "mystery", []byte("x")); err == nil {
		t.Fatal("an unknown artifact kind was accepted")
	}
}

// TestArtifactOpensOnlyTheContentAddressedPath is the read side of the
// trust boundary: where a blob lives is derived from the row's digest,
// never read from the row, so a path that somehow landed in the row —
// a completion's leftover, a hand-edited store — cannot point the
// engine's readers at a file of its choosing.
func TestArtifactOpensOnlyTheContentAddressedPath(t *testing.T) {
	e, _ := newArtifactEnv(t, nil)
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	blob := []byte("the run's own transcript")
	ans, info := e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, blob)
	if ans != AnsOK {
		t.Fatalf("upload = %s, want ok", ans)
	}

	// A sentinel file outside the artifact tree, named by the row's
	// path: the read must not open it.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not for readers"), 0o644); err != nil {
		t.Fatalf("write the sentinel: %v", err)
	}
	e.exec("UPDATE artifacts SET path = ? WHERE id = ?", outside, info.ID)

	got, file, err := e.eng.Artifact(e.ctx, info.ID)
	if err != nil || got == nil || file == nil {
		t.Fatalf("read the artifact = %+v, %v; want the row's blob", got, err)
	}
	defer file.Close()
	data := make([]byte, len("not for readers")+len(blob))
	n, _ := file.Read(data)
	if string(data[:n]) != string(blob) {
		t.Errorf("the streamed bytes = %q, want the content-addressed blob, not the row's path", data[:n])
	}
	if got.Path == outside {
		t.Error("the artifact's reported path is the row's planted path")
	}

	// A digest that is not a content address names nothing at all.
	e.exec("UPDATE artifacts SET sha256 = '../../etc/passwd' WHERE id = ?", info.ID)
	got, file, err = e.eng.Artifact(e.ctx, info.ID)
	if err != nil || got != nil || file != nil {
		t.Errorf("read with a planted digest = %+v, %v; want a silent not-found", got, err)
	}

	// A blob that is gone — pruned, lost — is a not-found too, not a
	// fault of the engine's.
	e2, dataDir2 := newArtifactEnv(t, nil)
	e2.submit(e2.work())
	g2 := e2.leaseAny(e2.runner1)
	_, info2 := e2.upload(g2.LeaseOwner, g2.JobID, g2.LeaseID, ArtifactTranscript, []byte("short-lived"))
	if err := os.Remove(filepath.Join(dataDir2, "artifacts", info2.SHA256[:2], info2.SHA256)); err != nil {
		t.Fatalf("remove the blob: %v", err)
	}
	got, file, err = e2.eng.Artifact(e2.ctx, info2.ID)
	if err != nil || got != nil || file != nil {
		t.Errorf("read of a missing blob = %+v, %v; want a silent not-found", got, err)
	}
}

// TestPruneAppliesRetention is §9's retention: old messages and
// artifacts go — the blobs with them — the event log stays whole, and
// what is younger than the cut stays.
func TestPruneAppliesRetention(t *testing.T) {
	e, dataDir := newArtifactEnv(t, nil)

	// One old run: submit, lease, a transcript, and the completion's
	// worker result message.
	e.submit(e.work())
	g := e.leaseAny(e.runner1)
	transcript := []byte("the old run's transcript")
	e.upload(g.LeaseOwner, g.JobID, g.LeaseID, ArtifactTranscript, transcript)
	e.complete(g, Completion{Result: `{"status":"done","summary":"old news"}`})
	sum := sha256.Sum256(transcript)
	sha := hex.EncodeToString(sum[:])
	blob := filepath.Join(dataDir, "artifacts", sha[:2], sha)

	// A second artifact of the same content, younger than the cut will
	// be: the blob must survive the old row's pruning, because the
	// young row still references it.
	e.clock.Advance(time.Hour)
	e.exec(`INSERT INTO artifacts (job_id, kind, path, size, sha256, created_at)
		VALUES (?, 'check_log', ?, ?, ?, ?)`, g.JobID, blob, len(transcript), sha, fmtTime(e.clock.Now()))

	// A message of the old era goes with the old rows; one younger than
	// the cut stays.
	e.exec(`INSERT INTO messages (project_id, kind, author, content, created_at)
		VALUES (?, 'note', 'engine', 'an old note', ?)`, e.project, fmtTime(e.clock.Now().Add(-time.Minute)))
	e.exec(`INSERT INTO messages (project_id, kind, author, content, created_at)
		VALUES (?, 'note', 'engine', 'a young note', ?)`, e.project, fmtTime(e.clock.Now().Add(time.Minute)))

	cut := e.clock.Now()
	eventsBefore := e.int("SELECT COUNT(*) FROM events")

	rep, err := e.eng.Prune(e.ctx, cut)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// The old artifact row, and the three old messages — the task's
	// spec, the run's result, the note; the file stays, because the
	// young row shares it.
	if rep.Artifacts != 1 || rep.Messages != 3 || rep.Files != 0 {
		t.Errorf("prune report = %+v, want one artifact, three messages, no file removed", rep)
	}
	if n := e.int("SELECT COUNT(*) FROM events"); n != eventsBefore {
		t.Errorf("events = %d after pruning, want the same %d: the log is never pruned", n, eventsBefore)
	}
	if n := e.int("SELECT COUNT(*) FROM messages"); n != 1 {
		t.Errorf("messages = %d, want the young note", n)
	}
	if n := e.int("SELECT COUNT(*) FROM artifacts"); n != 1 {
		t.Errorf("artifacts = %d, want the young row", n)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Errorf("the blob the young row shares was removed: %v", err)
	}

	// Pruning past the young row takes the file with it: nothing
	// references the content anymore.
	e.clock.Advance(time.Hour)
	rep, err = e.eng.Prune(e.ctx, e.clock.Now())
	if err != nil {
		t.Fatalf("prune again: %v", err)
	}
	if rep.Files != 1 {
		t.Errorf("prune report = %+v, want the unreferenced blob removed", rep)
	}
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Errorf("the blob = %v, want it gone", err)
	}
	// A job whose transcript was pruned loses the pointer, not itself.
	if got := e.str("SELECT COALESCE(transcript_id, '') FROM jobs WHERE id = ?", g.JobID); got != "" {
		t.Errorf("the job still points at a pruned transcript (%s)", got)
	}
}

// TestPruneSweepsStaleUploadTemps covers the janitor half of the prune:
// an upload temp a crashed writer left behind goes once it is older
// than any upload still in flight; a young one — a live upload's —
// stays, whatever the prune takes from the store.
func TestPruneSweepsStaleUploadTemps(t *testing.T) {
	e, dataDir := newArtifactEnv(t, nil)

	root := filepath.Join(dataDir, "artifacts", "ab")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("make the artifact directory: %v", err)
	}
	stale := filepath.Join(root, ".blob-crashed-mid-write")
	fresh := filepath.Join(root, ".blob-upload-in-flight")
	for _, path := range []string{stale, fresh} {
		if err := os.WriteFile(path, []byte("half a blob"), 0o644); err != nil {
			t.Fatalf("plant %s: %v", path, err)
		}
	}
	aged := e.clock.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, aged, aged); err != nil {
		t.Fatalf("age the stale temp: %v", err)
	}

	rep, err := e.eng.Prune(e.ctx, e.clock.Now())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rep.Files != 1 {
		t.Errorf("prune report = %+v, want the stale temp counted as a file", rep)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale temp = %v, want it gone", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the young temp = %v, want a live upload's temp kept", err)
	}
}

// TestSweepAppliesRetention runs the knob end to end: a configured
// window prunes on the maintenance pass; the default keeps everything.
func TestSweepAppliesRetention(t *testing.T) {
	e, _ := newArtifactEnv(t, func(c *Config) { c.Retention = time.Hour })
	e.exec(`INSERT INTO messages (project_id, kind, author, content, created_at)
		VALUES (?, 'note', 'engine', 'an old note', ?)`, e.project, fmtTime(e.clock.Now().Add(-2*time.Hour)))
	e.exec(`INSERT INTO messages (project_id, kind, author, content, created_at)
		VALUES (?, 'note', 'engine', 'a young note', ?)`, e.project, fmtTime(e.clock.Now()))
	if _, err := e.eng.Sweep(e.ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := e.int("SELECT COUNT(*) FROM messages"); n != 1 {
		t.Errorf("messages after the sweeping prune = %d, want the young one", n)
	}

	// The default keeps everything (§9).
	keep, _ := newArtifactEnv(t, nil)
	keep.exec(`INSERT INTO messages (project_id, kind, author, content, created_at)
		VALUES (?, 'note', 'engine', 'an old note', ?)`, keep.project, fmtTime(keep.clock.Now().Add(-100*time.Hour)))
	if _, err := keep.eng.Sweep(keep.ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := keep.int("SELECT COUNT(*) FROM messages"); n != 1 {
		t.Errorf("messages after a default sweep = %d, want everything kept", n)
	}
}
