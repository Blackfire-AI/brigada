package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/engine"
	"github.com/Blackfire-AI/brigada/internal/mcp"
)

// runAsk is `brigada ask` (§4.2 item 3): the user's message to the
// foreman, sent without attaching — the words as arguments, or "-" to
// read them from stdin — and the answer says what happened to it.
func runAsk(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("ask",
		"brigada ask [--repo URL] <message...> | -",
		"Send the foreman a message, or read it from standard input with -.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	repo := fs.String("repo", "", "the project's repo URL; empty takes the working directory's origin")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	words := fs.Args()
	if len(words) == 0 {
		return usagef(stderr, "usage: brigada ask [--repo URL] <message...> (or - to read it from stdin)")
	}
	text := strings.Join(words, " ")
	if len(words) == 1 && words[0] == "-" {
		// One byte past the engine's bound is enough to be refused with
		// the right words; nothing more is read.
		data, err := io.ReadAll(io.LimitReader(os.Stdin, engine.UserMessageLimit+1))
		if err != nil {
			return fail(stderr, fmt.Errorf("read the message from stdin: %w", err))
		}
		text = string(data)
	}
	projectRepo, err := askRepo(*repo)
	if err != nil {
		return fail(stderr, err)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", false, stderr)
	if code != 0 {
		return code
	}
	// A command typed by a person is deliberate each time: no key.
	res, err := client.AskForeman(ctx, projectRepo, text, "")
	if err != nil {
		if isNotFound(err) {
			if hint := knownProjectsHint(ctx, client); hint != "" {
				err = fmt.Errorf("%w; %s", err, hint)
			}
		}
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, mcp.AskOutcomeLine(res))
	return 0
}

// askRepo names the project an ask goes to: the --repo URL when given,
// else the working directory's origin, as attach and project foreman
// take it. Outside a repository, or in one with no origin, it is empty,
// which the engine answers with its only project.
func askRepo(repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find the working directory: %w", err)
	}
	origin, err := gitOriginURL(dir)
	if err != nil {
		return "", nil
	}
	return origin, nil
}
