package release

import (
	"os"
	"strings"
	"testing"
)

// releaseWorkflow is the workflow that builds and publishes release files.
const releaseWorkflow = "../../.github/workflows/release.yml"

// attestStep is the provenance step, pinned by full commit SHA.
const attestStep = "uses: actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6"

// TestReleaseWorkflowAttestsArtifacts checks text, not YAML, so the test needs
// no YAML dependency. It fails when the release job stops signing the files
// in dist/SHA256SUMS, loses the permissions signing needs, or runs the
// attestation out of order.
func TestReleaseWorkflowAttestsArtifacts(t *testing.T) {
	raw, err := os.ReadFile(releaseWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	wf := string(raw)

	// The workflow default must stay read-only.
	const defaultPerms = "permissions:\n  contents: read\n\njobs:"
	if !strings.Contains(wf, defaultPerms) {
		t.Errorf("workflow default permissions must be `contents: read` before jobs:")
	}

	release := jobSection(t, wf, "  release:", "  homebrew:")

	for _, perm := range []string{
		"contents: write",
		"id-token: write",
		"attestations: write",
		"artifact-metadata: write",
	} {
		if !strings.Contains(release, perm) {
			t.Errorf("release job permissions missing %q", perm)
		}
	}

	if !strings.Contains(release, attestStep) {
		t.Errorf("release job missing step %q", attestStep)
	}
	if !strings.Contains(release, "subject-checksums: dist/SHA256SUMS") {
		t.Errorf("attest step must set `subject-checksums: dist/SHA256SUMS`")
	}
	// The checksum file that names the release artifacts must be written by the build.
	if !strings.Contains(release, "sha256sum brigada-* > SHA256SUMS") {
		t.Errorf("build step must write SHA256SUMS over the release artifacts")
	}

	build := strings.Index(release, "- name: build")
	attest := strings.Index(release, attestStep)
	publish := strings.Index(release, "- name: publish")
	if build < 0 || attest < 0 || publish < 0 {
		t.Fatalf("release job missing build, attest or publish step (indexes %d, %d, %d)", build, attest, publish)
	}
	if !(build < attest && attest < publish) {
		t.Errorf("attest step must run after build and before publish")
	}
}

// jobSection returns the text of one job, from its header up to the next job.
func jobSection(t *testing.T, wf, start, end string) string {
	t.Helper()
	i := strings.Index(wf, start)
	if i < 0 {
		t.Fatalf("workflow has no job %q", strings.TrimSpace(start))
	}
	rest := wf[i:]
	if j := strings.Index(rest, end); j >= 0 {
		return rest[:j]
	}
	return rest
}
