package main

import (
	"fmt"
	"strings"
	"testing"
)

var tarballs = []string{
	"brigada-darwin-arm64.tar.gz",
	"brigada-darwin-amd64.tar.gz",
	"brigada-linux-arm64.tar.gz",
	"brigada-linux-amd64.tar.gz",
}

func sampleSums() string {
	var sums strings.Builder
	for i, name := range tarballs {
		fmt.Fprintf(&sums, "%s  %s\n", strings.Repeat(fmt.Sprint(i+1), 64), name)
	}
	// The release also includes raw binaries; they are not formula inputs.
	fmt.Fprintf(&sums, "%s  brigada-linux-amd64\n", strings.Repeat("a", 64))
	return sums.String()
}

func TestGenerate(t *testing.T) {
	got, err := generate(sampleSums(), "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"class Brigada < Formula\n",
		`  desc "Coordinate coding agents with checks and cross-reviews"`,
		`  homepage "https://github.com/Blackfire-AI/brigada"`,
		`  license "Apache-2.0"`,
		"  def install\n    bin.install \"brigada\"\n  end",
		"  test do\n    assert_match version.to_s, shell_output(\"#{bin}/brigada version\")\n  end\nend\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formula missing %q", want)
		}
	}
	if strings.Contains(got, `  version "`) {
		t.Error("formula carries an explicit version; brew audits it as redundant with the version scanned from the URL")
	}
	for i, name := range tarballs {
		os := "macos"
		if i >= 2 {
			os = "linux"
		}
		arch := "arm"
		if i%2 == 1 {
			arch = "intel"
		}
		want := fmt.Sprintf("    on_%s do\n      url \"https://github.com/Blackfire-AI/brigada/releases/download/v1.2.3/%s\"\n      sha256 \"%s\"\n    end", arch, name, strings.Repeat(fmt.Sprint(i+1), 64))
		start := strings.Index(got, "  on_"+os+" do\n")
		if start < 0 || !strings.Contains(strings.SplitN(got[start:], "\n  end", 2)[0], want) {
			t.Errorf("%s block missing %q", os, want)
		}
	}
	if strings.Count(got, "      url ") != 4 || strings.Count(got, "      sha256 ") != 4 {
		t.Error("want exactly four release tarballs and checksums")
	}
}

func TestGenerateMissingTarball(t *testing.T) {
	for i, name := range tarballs {
		t.Run(name, func(t *testing.T) {
			line := fmt.Sprintf("%s  %s\n", strings.Repeat(fmt.Sprint(i+1), 64), name)
			got, err := generate(strings.ReplaceAll(sampleSums(), line, ""), "1.2.3")
			if err == nil || !strings.Contains(err.Error(), "missing tarball "+name+" in SHA256SUMS") || got != "" {
				t.Fatalf("got %q, %v; want missing-tarball error and no formula", got, err)
			}
		})
	}
}

func TestGenerateInvalidInput(t *testing.T) {
	if _, err := generate(sampleSums(), ""); err == nil {
		t.Error("empty version: want error")
	}
	if _, err := generate(strings.ReplaceAll(sampleSums(), strings.Repeat("1", 64), "bad"), "1.2.3"); err == nil {
		t.Error("invalid checksum: want error")
	}
}
