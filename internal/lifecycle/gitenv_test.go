package lifecycle

import (
	"os"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestMain turns off git's automatic garbage collection and background
// maintenance for every git command the tests and the code under test
// spawn: its writes into .git race the TempDir cleanup (see
// testsupport.DisableGitAutoMaintenance). It serves the external
// _test package's tests too: a directory runs one TestMain.
func TestMain(m *testing.M) {
	testsupport.DisableGitAutoMaintenance()
	os.Exit(m.Run())
}
