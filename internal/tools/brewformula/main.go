// Command brewformula prints the complete Homebrew formula for a release.
//
//	go run ./internal/tools/brewformula -version 1.0.0 -sums dist/SHA256SUMS
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
)

func main() {
	version := flag.String("version", "", "release version without the leading v")
	sums := flag.String("sums", "dist/SHA256SUMS", "path to release SHA256SUMS")
	flag.Parse()
	data, err := os.ReadFile(*sums)
	if err == nil {
		var formula string
		formula, err = generate(string(data), *version)
		if err == nil {
			fmt.Print(formula)
			return
		}
	}
	fmt.Fprintln(os.Stderr, "brewformula:", err)
	os.Exit(1)
}

func generate(sums, version string) (string, error) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`).MatchString(version) {
		return "", fmt.Errorf("invalid release version %q (want a version without the leading v)", version)
	}
	checksums := make(map[string]string)
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			checksums[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}
	var formula strings.Builder
	fmt.Fprintf(&formula, `class Brigada < Formula
  desc "Coordinate coding agents with checks and cross-reviews"
  homepage "https://github.com/Blackfire-AI/brigada"
  license "Apache-2.0"
`)
	for _, platform := range []struct{ os, block string }{{"darwin", "macos"}, {"linux", "linux"}} {
		fmt.Fprintf(&formula, "\n  on_%s do\n", platform.block)
		for i, arch := range []struct{ name, block string }{{"arm64", "arm"}, {"amd64", "intel"}} {
			name := "brigada-" + platform.os + "-" + arch.name + ".tar.gz"
			sum, ok := checksums[name]
			if !ok {
				return "", fmt.Errorf("missing tarball %s in SHA256SUMS", name)
			}
			if decoded, err := hex.DecodeString(sum); err != nil || len(decoded) != 32 {
				return "", fmt.Errorf("invalid SHA256 for %s in SHA256SUMS", name)
			}
			if i > 0 {
				formula.WriteString("\n")
			}
			fmt.Fprintf(&formula, `    on_%s do
      url "https://github.com/Blackfire-AI/brigada/releases/download/v%s/%s"
      sha256 "%s"
    end
`, arch.block, version, name, sum)
		}
		formula.WriteString("  end\n")
	}
	formula.WriteString(`
  def install
    bin.install "brigada"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/brigada version")
  end
end
`)
	return formula.String(), nil
}
