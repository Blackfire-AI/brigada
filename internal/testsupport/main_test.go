package testsupport

import (
	"os"
	"testing"
)

// TestMain turns off git's automatic garbage collection and background
// maintenance for every git command the tests and the code under test
// spawn: its writes into .git race the TempDir cleanup (see
// DisableGitAutoMaintenance), and removes the brigada binary the tests
// built.
func TestMain(m *testing.M) {
	DisableGitAutoMaintenance()
	code := m.Run()
	RemoveBuiltBrigada()
	os.Exit(code)
}
