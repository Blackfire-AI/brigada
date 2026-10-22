package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden THIRD_PARTY_NOTICES.md")

// TestThirdPartyNoticesGolden keeps the checked-in THIRD_PARTY_NOTICES.md
// in step with the dependency tree: it regenerates the file through
// go-licenses and compares it against the golden at the repository
// root. Rewrite the golden with:
//
//	go test ./internal/tools/licenses -run TestThirdPartyNoticesGolden -update
func TestThirdPartyNoticesGolden(t *testing.T) {
	golden, err := filepath.Abs(filepath.Join("..", "..", "..", "THIRD_PARTY_NOTICES.md"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := run(&buf); err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Error("THIRD_PARTY_NOTICES.md is stale; regenerate with: go test ./internal/tools/licenses -run TestThirdPartyNoticesGolden -update")
	}
}
