package tools

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// staleMarker is what the golden test reports when the checked-in notices
// differ from the regenerated ones. That is a real failure, never a skip.
const staleMarker = "is stale"

// sandboxMarkers are what the generator prints when it cannot start or
// resolve its modules: no network, no module cache, no proxy. They say
// nothing about the notices, so the test skips on them outside CI.
var sandboxMarkers = []string{
	"dial tcp",
	"no such host",
	"network is unreachable",
	"connection refused",
	"i/o timeout",
	"GOPROXY",
	"module lookup disabled",
	"proxy.golang.org",
	"go list -m -json all",
	"cannot find module",
	"operation not permitted",
	"permission denied",
}

// TestThirdPartyNoticesFresh runs the nested licences module's golden
// test, so the root-module checks catch a THIRD_PARTY_NOTICES.md gone
// stale against go.mod: a nested module is invisible to the root ./...
// patterns, and nothing else in CI would run its tests.
//
// The generator needs the go tool, the module cache and network. When it
// cannot get them the test skips, except under CI=true, where it fails so
// that CI never passes without checking the notices.
func TestThirdPartyNoticesFresh(t *testing.T) {
	cmd := exec.Command("go", "test", "-count=1", "-run", "TestThirdPartyNoticesGolden", ".")
	cmd.Dir = "licenses"
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	if os.Getenv("CI") == "true" || !cannotRunGenerator(err, out) {
		t.Fatalf("third-party notices are stale or the generator failed: %v\n%s", err, out)
	}
	t.Skipf("third-party notices not checked: the generator cannot run here (go tool, module cache or network unavailable): %v", err)
}

// cannotRunGenerator reports whether the failure comes from the generator
// being unable to start or resolve its modules, rather than from the
// notices being stale or a licence being refused.
func cannotRunGenerator(err error, out []byte) bool {
	if strings.Contains(string(out), staleMarker) {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	for _, m := range sandboxMarkers {
		if strings.Contains(string(out), m) {
			return true
		}
	}
	return false
}
