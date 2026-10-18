package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
)

const (
	initCheckIntro = "\nStep 3 of 3: how brigada tests the work\n" +
		"  When an agent finishes a task, brigada runs one command in that task's\n" +
		"  own copy of the repo (never in your working copy):\n" +
		"    passes (exit 0)  the work goes on to review by another model family\n" +
		"    fails            the work goes back to the agent, with the output\n" +
		"  Use the command you run yourself to be sure nothing broke: tests,\n" +
		"  lint, type checks. Claude workers may run it too before handing in.\n"
	initCheckOutro      = "\n  To change it later: edit default_check in .brigada/project.yaml and\n  run brigada project import, or run brigada init again.\n"
	initCheckCurrent    = "current, from .brigada/project.yaml"
	initCheckTypedRetry = "  type a command; try again.\n"
)

// initCheckEntries is the menu: the project's current check first when it
// has one, then the candidates without a duplicate of it.
func initCheckEntries(candidates []checkCandidate, current, currentTools string) []checkCandidate {
	if current == "" {
		return candidates
	}
	entries := []checkCandidate{{Command: current, Note: initCheckCurrent, Tools: currentTools}}
	for _, c := range candidates {
		if c.Command != current {
			entries = append(entries, c)
		}
	}
	return entries
}

// initCheckTools is the worker tool scope that lets command run: the one a
// menu entry carries (the candidate's when the entry has none), else a scope
// for the command itself. It is empty when no valid scope can be built.
func initCheckTools(command string, entries, candidates []checkCandidate) string {
	for _, e := range entries {
		if e.Command != command {
			continue
		}
		if e.Tools != "" {
			return e.Tools
		}
		break
	}
	for _, c := range candidates {
		if c.Command == command {
			return c.Tools
		}
	}
	// "+" joins scopes and parentheses end the pattern: no scope for those.
	pattern := "Bash(" + command + ":*)"
	if _, err := lifecycle.ParseTools(pattern); err != nil || strings.ContainsAny(command, "+()") {
		return ""
	}
	return pattern
}

// initCheckKnown reports whether command is a menu entry.
func initCheckKnown(command string, entries []checkCandidate) bool {
	for _, e := range entries {
		if e.Command == command {
			return true
		}
	}
	return false
}

// askInitCheck prints Step 3 and asks for the check. It returns the command
// chosen; exhausted is true when three answers were all unusable.
func askInitCheck(w io.Writer, ask func(prompt, flags string) (string, error), entries []checkCandidate) (command string, exhausted bool, err error) {
	fmt.Fprint(w, initCheckIntro)
	prompt := "  Check command: "
	if len(entries) == 0 {
		fmt.Fprint(w, "\n  Found in this repo: nothing I recognise\n")
	} else {
		header := "Found in this repo:"
		if entries[0].Note == initCheckCurrent {
			header = "Choices:"
		}
		fmt.Fprintf(w, "\n  %s\n", header)
		width := 0
		for _, e := range entries {
			if n := utf8.RuneCountInString(e.Command); n > width {
				width = n
			}
		}
		for i, e := range entries {
			prefix := fmt.Sprintf("    %2d  %-*s  ", i+1, width, e.Command)
			fmt.Fprintf(w, "%s%s\n", prefix, truncateNote(e.Note, 79-utf8.RuneCountInString(prefix)))
		}
		prompt = fmt.Sprintf("  Pick %s, or type a command [1]: ", initPickRange(len(entries)))
	}
	for attempt := 0; attempt < 3; attempt++ {
		answer, err := ask(prompt, "--check COMMAND")
		if err != nil {
			return "", false, err
		}
		switch {
		case answer == "" && len(entries) > 0:
			command = entries[0].Command
		case answer == "":
			fmt.Fprint(w, initCheckTypedRetry)
			continue
		case strings.Trim(answer, "0123456789") == "":
			n, _ := strconv.Atoi(answer)
			if len(entries) == 0 {
				fmt.Fprint(w, initCheckTypedRetry)
				continue
			}
			if n < 1 || n > len(entries) {
				fmt.Fprintf(w, "  pick %s or type a command; try again.\n", initPickRange(len(entries)))
				continue
			}
			command = entries[n-1].Command
		default:
			command = answer
		}
		fmt.Fprint(w, initCheckOutro)
		return command, false, nil
	}
	return "", true, nil
}
