package main

// The read-scope hook command (design §4.1): the PreToolUse hook a
// foreman session's settings run before each Read, Grep and Glob. It is
// brigada's own plumbing — the session's settings invoke it, nobody
// else — so run dispatches it beside the command table, and it is in
// no usage text: the foreman's file tools stay confined to the
// session's one allowed root, the reading checkout, whatever the
// session believes.

import (
	"context"
	"fmt"
	"io"

	"github.com/Blackfire-AI/brigada/internal/adapter/claude"
)

// runReadScope reads one PreToolUse payload on stdin and refuses the
// call when it reads outside --root: exit 2 blocks the tool call, with
// the reason on stderr for the model to see, and exit 0 lets it
// through. The decision is claude.ReadScopeDeny's; this is only its
// plumbing. A payload that cannot even be read is refused the same way
// — the CLI treats any exit other than 2 as non-blocking, so a broken
// pipe is a blocked call, not an allowed one.
func runReadScope(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("readscope", "brigada readscope --root <dir>",
		"The foreman session's read-scope hook; the session's settings run it, nobody by hand.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	root := fs.String("root", "", "the one directory the session's file tools may read")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "readscope takes no positional arguments, got %q", extra)
	}
	if *root == "" {
		return usagef(stderr, "readscope requires --root <dir>")
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "brigada: the hook payload cannot be read; refusing the call")
		return 2
	}
	if reason, deny := claude.ReadScopeDeny(*root, payload); deny {
		fmt.Fprintln(stderr, reason)
		return 2
	}
	return 0
}
