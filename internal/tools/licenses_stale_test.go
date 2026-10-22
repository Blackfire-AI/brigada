package tools

import (
	"os/exec"
	"testing"
)

// TestThirdPartyNoticesFresh runs the nested licences module's golden
// test, so the root-module checks catch a THIRD_PARTY_NOTICES.md gone
// stale against go.mod: a nested module is invisible to the root ./...
// patterns, and nothing else in CI would run its tests.
func TestThirdPartyNoticesFresh(t *testing.T) {
	cmd := exec.Command("go", "test", "-count=1", "-run", "TestThirdPartyNoticesGolden", ".")
	cmd.Dir = "licenses"
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("third-party notices are stale or the generator failed: %v\n%s", err, out)
	}
}
