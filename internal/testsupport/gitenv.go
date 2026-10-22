package testsupport

import "os"

// DisableGitAutoMaintenance turns off git's automatic garbage collection
// and background maintenance for every git command this process spawns,
// including the ones the code under test starts. Git writes into .git
// in the background while those commands run, and that races a test's
// TempDir cleanup: RemoveAll can fail with "unlinkat .../.git: directory
// not empty" when git repacks the repository between the listing and
// the unlink. The two settings go through GIT_CONFIG_* so they reach
// every repository the tests create, including plain `git init` ones
// that no template config reaches.
//
// This is a process-level setting meant for TestMain, not t.Setenv:
// tests spawn git directly and through helpers, and each of those
// inherits the process environment.
func DisableGitAutoMaintenance() {
	os.Setenv("GIT_CONFIG_COUNT", "2")
	os.Setenv("GIT_CONFIG_KEY_0", "gc.auto")
	os.Setenv("GIT_CONFIG_VALUE_0", "0")
	os.Setenv("GIT_CONFIG_KEY_1", "maintenance.auto")
	os.Setenv("GIT_CONFIG_VALUE_1", "false")
}
