package main

// runKeptHold reports whether args are a call to one of the two hold
// subcommands that old Claude Code hooks still run: a Stop hook with
// `hold yield` and a SessionStart hook with `hold confirm`. Task 53 removed
// hold; these calls now do nothing and exit 0, so the hooks do not block
// sessions until the user edits their settings. Any other hold subcommand
// stays an unknown command.
func runKeptHold(args []string) bool {
	return len(args) > 1 && args[0] == "hold" && (args[1] == "yield" || args[1] == "confirm")
}
