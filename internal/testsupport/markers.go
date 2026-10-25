package testsupport

// AwaitRelease is the shell loop a scripted CLI holds itself open with
// until the test writes $BRIGADA_TEST_MARKERS/release. It also ends the
// script, with exit 1, once the test is gone: the marker directory
// removed (the test ended, failed or panicked and its TempDir went), or
// the process that started the script dead (the test binary timed out
// or was killed, so no cleanup ran). Without that, a test that never
// released left the script polling forever, reparented to init, holding
// whatever its stdout and stderr pointed at.
//
// $PPID is the shell's parent at its start: the scripts are started
// directly (exec.Command of the script), so it names the test binary or
// the runner that ran them.
const AwaitRelease = `while [ ! -f "$BRIGADA_TEST_MARKERS/release" ]; do [ -d "$BRIGADA_TEST_MARKERS" ] && kill -0 "$PPID" 2>/dev/null || exit 1; sleep 0.05; done`
