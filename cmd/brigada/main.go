// Command brigada is the one binary of the brigada engine: the client, the
// engine, the runner and the foreman's MCP bridge are all roles of this same
// binary. Only `version` is implemented so far; the rest arrive with the
// engine itself.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/Blackfire-AI/brigada/internal/version"
)

const usage = `brigada is the single binary of the brigada engine: client,
engine, runner and foreman are all this binary in different roles.

Usage:

	brigada <command> [arguments]

Commands:

	version    Print the brigada version and exit.
	help       Print usage and exit.
`

// run is the entry point of the brigada command. It writes to stdout and
// stderr and returns the process exit code, so it can be tested without
// spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "brigada %s\n", version.Version)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
