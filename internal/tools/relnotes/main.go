// Command relnotes prints the CHANGELOG.md section for a release tag.
//
//	go run ./internal/tools/relnotes CHANGELOG.md v1.0.0-rc.1
//
// It looks for a "## [1.0.0-rc.1]" heading and exits non-zero when there is
// none or the section is empty.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: relnotes CHANGELOG.md TAG")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "relnotes:", err)
		os.Exit(1)
	}
	notes, err := section(string(data), os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "relnotes:", err)
		os.Exit(1)
	}
	fmt.Println(notes)
}

// section returns the body of the changelog section for tag.
func section(changelog, tag string) (string, error) {
	version := strings.TrimPrefix(tag, "v")
	lines := strings.Split(changelog, "\n")
	for _, v := range []string{version} {
		prefix := "## [" + v + "]"
		start := -1
		for i, l := range lines {
			if strings.HasPrefix(l, prefix) {
				start = i + 1
				break
			}
		}
		if start < 0 {
			continue
		}
		end := len(lines)
		for i := start; i < len(lines); i++ {
			if strings.HasPrefix(lines[i], "## [") {
				end = i
				break
			}
		}
		body := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		if body == "" {
			return "", fmt.Errorf("section %s is empty", prefix)
		}
		return body, nil
	}
	return "", fmt.Errorf("no CHANGELOG section for %s", tag)
}
