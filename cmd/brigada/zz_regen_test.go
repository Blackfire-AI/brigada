package main

import (
	"flag"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/logo"
)

// regenInitGolden rewrites an init transcript golden when the package's
// -update flag is set, so the goldens are regenerated, not hand-edited.
// Run: go test ./cmd/brigada -run 'TestInitInteractiveTranscript$|TestInitStyledInteractiveTranscript' -update
func regenInitGolden(t *testing.T, longModels bool, forced []logo.Caps, out string) {
	t.Helper()
	if f := flag.Lookup("update"); f == nil || f.Value.String() != "true" || longModels {
		return
	}
	name := "init-interactive.golden"
	if len(forced) > 0 {
		caps := forced[0]
		switch {
		case caps.Color == logo.TrueColor:
			name = "init-interactive-truecolor.golden"
		case caps.UTF8:
			name = "init-interactive-16color.golden"
		default:
			name = "init-interactive-ascii.golden"
		}
	}
	// The test has changed directory by now; the source file's directory is fixed.
	_, source, _, _ := goruntime.Caller(0)
	path := filepath.Join(filepath.Dir(source), "testdata", name)
	if err := os.WriteFile(path, []byte(out), 0644); err != nil {
		t.Fatal(err)
	}
}
