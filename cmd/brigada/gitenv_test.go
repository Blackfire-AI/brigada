package main

import (
	"os"
	"testing"

	"github.com/Blackfire-AI/brigada/internal/testsupport"
)

// TestMain turns off git's automatic garbage collection and background
// maintenance for every git command the tests and the code under test
// spawn: its writes into .git race the TempDir cleanup (see
// testsupport.DisableGitAutoMaintenance), and removes the brigada binary
// the tests built.
func TestMain(m *testing.M) {
	testsupport.DisableGitAutoMaintenance()
	code := m.Run()
	testsupport.RemoveBuiltBrigada()
	os.Exit(code)
}
