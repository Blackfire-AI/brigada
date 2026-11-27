package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// releaseWorkflow is the workflow that builds and publishes release files.
const releaseWorkflow = "../../.github/workflows/release.yml"

// nfpmConfig is the nfpm config the packages step passes to nfpm explicitly.
const nfpmConfig = "../../.nfpm.yaml"

// attestStep is the provenance step, pinned by full commit SHA.
const attestStep = "uses: actions/attest@1e69f48acb82d1966a394da916b4c1698aa569d6"

// packageNames are the file names nfpm renders for a tag, which the workflow
// must find in dist/ and list in SHA256SUMS. The rpm version-release split is
// nfpm's: a prerelease keeps its tilde in the rpm Version and the default
// Release is 1, so no dash ever appears inside a Version. Each rc name was
// checked against real nfpm output; TestNfpmRendersPackageNames repeats that
// check whenever nfpm is on PATH.
var packageNames = []struct {
	tag, version, prerelease string
	files                    []string
}{
	{"v1.0.0", "1.0.0", "", []string{
		"brigada_1.0.0_amd64.deb", "brigada_1.0.0_arm64.deb",
		"brigada-1.0.0-1.x86_64.rpm", "brigada-1.0.0-1.aarch64.rpm",
	}},
	{"v1.0.0-rc.1", "1.0.0", "rc.1", []string{
		"brigada_1.0.0~rc.1_amd64.deb", "brigada_1.0.0~rc.1_arm64.deb",
		"brigada-1.0.0~rc.1-1.x86_64.rpm", "brigada-1.0.0~rc.1-1.aarch64.rpm",
	}},
}

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
	// The checksum file names every release artifact — tarballs, binaries and
	// the packages — and is written after nfpm so they are all attested.
	if !strings.Contains(release, "sha256sum brigada-* brigada_*.deb > SHA256SUMS") {
		t.Errorf("checksums step must write SHA256SUMS over the tarballs, binaries and packages")
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

// readRelease returns the release job's text, from its header to the next job.
func readRelease(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(releaseWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	return jobSection(t, string(raw), "  release:", "  homebrew:")
}

// stepRun returns the shell script of the named step of the release job.
func stepRun(t *testing.T, release, name string) string {
	t.Helper()
	i := strings.Index(release, "- name: "+name+"\n")
	if i < 0 {
		t.Fatalf("release job has no step %q", name)
	}
	lines := strings.Split(release[i:], "\n")
	var script []string
	in := false
	for _, l := range lines[1:] {
		if !in {
			if strings.TrimSpace(l) == "run: |" {
				in = true
			} else if strings.HasPrefix(l, "      - ") {
				break
			}
			continue
		}
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "          ") {
			break
		}
		script = append(script, strings.TrimPrefix(l, "          "))
	}
	if len(script) == 0 {
		t.Fatalf("step %q has no run script", name)
	}
	return strings.Join(script, "\n") + "\n"
}

// TestReleaseWorkflowInstallsNfpmPinned pins the gitleaks pattern: a versioned
// release archive and the checksums file, checked by sha256sum -c.
func TestReleaseWorkflowInstallsNfpmPinned(t *testing.T) {
	release := readRelease(t)
	step := stepRun(t, release, "install nfpm")
	for _, want := range []string{
		"NFPM_VERSION:",
		"https://github.com/goreleaser/nfpm/releases/download/v${NFPM_VERSION}",
		"curl -fsSL -o \"$dir/$archive\"",
		"checksums.txt",
		"sha256sum -c -",
	} {
		if !strings.Contains(release, want) {
			t.Errorf("nfpm acquisition missing %q", want)
		}
	}
	if strings.Index(step, "sha256sum -c") > strings.Index(step, "tar -xzf") {
		t.Errorf("nfpm archive must be verified before it is unpacked")
	}
	raw, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "nfpm") {
		t.Errorf("nfpm must stay out of go.mod")
	}
}

// TestReleaseWorkflowPackagesBeforeSums pins the ordering that makes the
// packages checksummed and attested: nfpm runs before SHA256SUMS is written,
// the build step no longer writes it, and the packages read dist/brigada-linux-<arch>.
func TestReleaseWorkflowPackagesBeforeSums(t *testing.T) {
	release := readRelease(t)
	install := strings.Index(release, "- name: install nfpm")
	packages := strings.Index(release, "- name: packages")
	sums := strings.Index(release, "- name: checksums")
	attest := strings.Index(release, attestStep)
	if install < 0 || packages < 0 || sums < 0 || attest < 0 {
		t.Fatalf("missing step (install nfpm %d, packages %d, checksums %d, attest %d)", install, packages, sums, attest)
	}
	if !(install < packages && packages < sums && sums < attest) {
		t.Errorf("order must be install nfpm < packages < checksums < attest")
	}
	if got := strings.Count(release, "> SHA256SUMS"); got != 1 {
		t.Errorf("SHA256SUMS written %d times, want once, after nfpm", got)
	}
	if strings.Contains(stepRun(t, release, "build"), "SHA256SUMS") {
		t.Errorf("build step must not write SHA256SUMS before nfpm runs")
	}
	script := stepRun(t, release, "packages")
	if !strings.Contains(script, "--config .nfpm.yaml") {
		t.Errorf("packages step must pass .nfpm.yaml explicitly")
	}
	if strings.Contains(script, "stage/brigada") {
		t.Errorf("packages must not read stage/brigada, which the build loop overwrites")
	}
	if !strings.Contains(script, "--target dist/") {
		t.Errorf("packages must be written into dist/ so gh release create dist/* attaches them")
	}
	cfg, err := os.ReadFile(nfpmConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "src: dist/brigada-linux-${ARCH}") {
		t.Errorf(".nfpm.yaml must package dist/brigada-linux-<arch>")
	}
}

// TestNfpmConfig pins the package metadata.
func TestNfpmConfig(t *testing.T) {
	raw, err := os.ReadFile(nfpmConfig)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(raw)
	for _, want := range []string{
		"name: brigada\n",
		"arch: ${ARCH}\n",
		"platform: linux\n",
		"license: Apache-2.0\n",
		"homepage: https://github.com/Blackfire-AI/brigada\n",
		"maintainer: Evgeny Dolgov <504068+the-mad-dog@users.noreply.github.com>\n",
		"version: ${VERSION}\n",
		"prerelease: ${PRERELEASE}\n",
		"depends:\n  - git\n",
		"src: dist/brigada-linux-${ARCH}\n",
		"dst: /usr/bin/brigada\n",
		"expand: true\n",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf(".nfpm.yaml missing %q", want)
		}
	}
	if strings.Contains(cfg, "version_schema") {
		t.Errorf(".nfpm.yaml must keep nfpm's default semver schema, which renders the deb tilde")
	}
}

// TestPackagesStepSplitsTag runs the packages step's tag split for a final and
// an rc tag, with a stub nfpm that records its environment.
func TestPackagesStepSplitsTag(t *testing.T) {
	script := stepRun(t, readRelease(t), "packages")
	for _, c := range packageNames {
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		if err := os.Mkdir(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		stub := "#!/bin/sh\necho \"$ARCH $VERSION [$PRERELEASE] $*\" >> \"" + filepath.Join(dir, "calls") + "\"\n"
		if err := os.WriteFile(filepath.Join(bin, "nfpm"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "GITHUB_REF_NAME="+c.tag)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", c.tag, err, out)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "calls"))
		if err != nil {
			t.Fatal(err)
		}
		calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(calls) != 4 {
			t.Fatalf("%s: nfpm ran %d times, want 4 (deb and rpm for two arches): %q", c.tag, len(calls), calls)
		}
		for _, call := range calls {
			if !strings.Contains(call, " "+c.version+" ["+c.prerelease+"] ") {
				t.Errorf("%s: nfpm call %q, want version %q prerelease %q", c.tag, call, c.version, c.prerelease)
			}
		}
	}
}

// TestChecksumsListPackages runs the checksums step over a dist/ holding the
// tarballs, binaries and the packages nfpm names, and requires every package
// in SHA256SUMS: the sums write has to follow nfpm or they are not attested.
func TestChecksumsListPackages(t *testing.T) {
	script := stepRun(t, readRelease(t), "checksums")
	for _, c := range packageNames {
		root := t.TempDir()
		dist := filepath.Join(root, "dist")
		if err := os.Mkdir(dist, 0o755); err != nil {
			t.Fatal(err)
		}
		files := append([]string{}, c.files...)
		for _, arch := range []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"} {
			files = append(files, "brigada-"+arch, "brigada-"+arch+".tar.gz")
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(dist, f), []byte(f), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", c.tag, err, out)
		}
		sums, err := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if !strings.Contains(string(sums), "  "+f+"\n") {
				t.Errorf("%s: SHA256SUMS lacks %q:\n%s", c.tag, f, sums)
			}
		}
		if strings.Contains(string(sums), "SHA256SUMS") {
			t.Errorf("%s: SHA256SUMS lists itself", c.tag)
		}
	}
}

// TestNfpmRendersPackageNames runs the packages step with the real nfpm, when
// it is installed, and requires exactly the pinned file names.
func TestNfpmRendersPackageNames(t *testing.T) {
	nfpm, err := exec.LookPath("nfpm")
	if err != nil {
		t.Skip("nfpm is not on PATH")
	}
	script := stepRun(t, readRelease(t), "packages")
	cfg, err := os.ReadFile(nfpmConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range packageNames {
		root := t.TempDir()
		dist := filepath.Join(root, "dist")
		if err := os.Mkdir(dist, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, arch := range []string{"amd64", "arm64"} {
			if err := os.WriteFile(filepath.Join(dist, "brigada-linux-"+arch), []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, ".nfpm.yaml"), cfg, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(nfpm)+":"+os.Getenv("PATH"), "GITHUB_REF_NAME="+c.tag)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", c.tag, err, out)
		}
		entries, err := os.ReadDir(dist)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "brigada") && (strings.HasSuffix(e.Name(), ".deb") || strings.HasSuffix(e.Name(), ".rpm")) {
				got[e.Name()] = true
			}
		}
		if len(got) != len(c.files) {
			t.Errorf("%s: nfpm wrote %v, want %v", c.tag, got, c.files)
		}
		for _, f := range c.files {
			if !got[f] {
				t.Errorf("%s: nfpm did not write %q (wrote %v)", c.tag, f, got)
			}
		}
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

// TestInstallJobChecksPackagesInContainers pins the container install check:
// it needs the release, runs in the three distro images, installs git and curl
// before using them, verifies checksums before installing, and runs
// brigada version last.
func TestInstallJobChecksPackagesInContainers(t *testing.T) {
	raw, err := os.ReadFile(releaseWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	job := jobSection(t, string(raw), "\n  install:\n", "\n  \x00")
	for _, want := range []string{
		"needs: release",
		"contents: read",
		"runs-on: ubuntu-latest",
		"image: ${{ matrix.image }}",
		"image: debian:stable-slim",
		"image: ubuntu:latest",
		"image: fedora:latest",
	} {
		if !strings.Contains(job, want) {
			t.Errorf("install job lacks %q", want)
		}
	}
	if strings.Contains(job, "actions/checkout") {
		t.Error("install job must not use actions/checkout")
	}
	for _, line := range strings.Split(job, "\n") {
		if i := strings.Index(line, "uses:"); i >= 0 && !strings.Contains(line, "@") {
			t.Errorf("unpinned action: %s", line)
		}
	}
	// Each image's bootstrap installs git and curl with its own package manager.
	for _, want := range []string{
		"bootstrap: apt-get update && apt-get install -y git curl",
		"bootstrap: dnf install -y git curl",
	} {
		if !strings.Contains(job, want) {
			t.Errorf("install job lacks bootstrap %q", want)
		}
	}
	if n := strings.Count(job, "bootstrap: "); n != 3 {
		t.Errorf("want one bootstrap per image (3), found %d", n)
	}
	for _, want := range []string{"install: apt-get install -y", "install: dnf install -y"} {
		if !strings.Contains(job, want) {
			t.Errorf("install job lacks %q", want)
		}
	}
	idx := func(s string) int {
		i := strings.Index(job, s)
		if i < 0 {
			t.Fatalf("install job lacks %q", s)
		}
		return i
	}
	boot, curl := idx("- name: bootstrap"), idx("curl --fail")
	sums, inst, ver := idx("sha256sum -c"), idx("${{ matrix.install }}"), idx("run: brigada version")
	if !(boot < curl && curl < sums && sums < inst && inst < ver) {
		t.Errorf("order must be bootstrap < download < checksum < install < version: %d %d %d %d %d", boot, curl, sums, inst, ver)
	}
}
