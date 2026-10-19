package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/term"
)

const (
	initCheckCurrent    = "current, from .brigada/project.yaml"
	initCheckTypedRetry = "  type a command, or - for no check; try again.\n"
)

// initCheckEntries records the choices: the project's current check first when it
// has one, then the candidates without a duplicate of it, then no check.
func initCheckEntries(candidates []checkCandidate, current, currentTools string, hasProject bool) []checkCandidate {
	var entries []checkCandidate
	if current != "" {
		entries = append(entries, checkCandidate{Command: current, Note: initCheckCurrent, Tools: currentTools})
	}
	for _, c := range candidates {
		if c.Command != current {
			entries = append(entries, c)
		}
	}
	none := checkCandidate{}
	if hasProject && current == "" {
		none.Note = "current"
	}
	return append(entries, none)
}

func initCheckDefault(entries []checkCandidate) int {
	for i, e := range entries {
		if _, tags := initNoteTags(e.Note); tags == "current" {
			return i
		}
	}
	return 0
}

// initCheckTools is the worker tool scope that lets command run: the one a
// choice carries (the candidate's when the entry has none), else a scope
// for the command itself. No check grants only file tools, plus the Go
// defaults in a Go repository. An unrepresentable scope grants file tools only.
func initCheckTools(command string, entries, candidates []checkCandidate) string {
	if command == "" {
		for _, c := range candidates {
			if c.Command == "go test ./..." {
				return c.Tools
			}
		}
		return "none"
	}
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
		return "none"
	}
	return pattern
}

// askInitCheck prints Step 4's suggestions and asks for the command itself.
// Enter keeps the current project check on a rerun, else the best suggestion.
// exhausted is true when three answers were all unusable.
func askInitCheck(w io.Writer, ask func(prompt, flags string) (string, error), entries, candidates []checkCandidate) (command string, exhausted bool, err error) {
	fmt.Fprintln(w)
	initStep(w, 4, "the check", "  A command that must succeed (exit 0) before a task's work is accepted.\n")
	fmt.Fprintln(w)
	defaultEntry := entries[initCheckDefault(entries)]
	_, currentTag := initNoteTags(defaultEntry.Note)
	current := currentTag == "current"
	if current && !slices.ContainsFunc(candidates, func(c checkCandidate) bool { return c.Command == defaultEntry.Command }) {
		name := defaultEntry.Command
		if name == "" {
			name = "no check"
		}
		prefix := "    " + name + "  "
		printInitRow(w, prefix, "    ", "from .brigada/project.yaml", "current")
	}
	if len(candidates) == 0 {
		fmt.Fprintln(w, "  Found nothing brigada recognises.")
	} else {
		var files []string
		width := 0
		for _, c := range candidates {
			if c.File != "" && !slices.Contains(files, c.File) {
				files = append(files, c.File)
			}
			width = max(width, term.VisibleWidth(c.Command))
		}
		word := "suggestion is"
		if len(candidates) > 1 {
			word = "suggestions are"
		}
		printInitWrapped(w, "  ", "    ", "Found "+strings.Join(files, ", ")+", so the "+word+":")
		for _, c := range candidates {
			tag := ""
			if current && c.Command == defaultEntry.Command {
				tag = "current"
			}
			prefix := "    " + initPad(c.Command, width) + "  "
			room := initPresentation(w).style.Width() - 1 - term.VisibleWidth(prefix)
			if tag != "" {
				room -= term.VisibleWidth(tag) + 2
			}
			note := truncateNote(c.Note, room)
			printInitRow(w, prefix, "    ", note, tag)
		}
	}
	defaultAnswer := defaultEntry.Command
	if defaultAnswer == "" {
		defaultAnswer = "-"
	}
	prompt := fmt.Sprintf("  Check command [%s]: ", defaultAnswer)
	if len(candidates) == 0 && defaultAnswer == "-" {
		prompt = "  Check command, or - for none [-]: "
	}
	// A saved command can be longer than the terminal. Wrap the prompt as
	// well as the suggestions without changing the command Enter selects.
	if term.VisibleWidth(strings.TrimRight(prompt, " ")) >= initPresentation(w).style.Width() {
		var wrapped strings.Builder
		printInitWrapped(initOutput{Writer: &wrapped, style: initPresentation(w).style}, "  ", "    ", strings.TrimSpace(prompt))
		prompt = strings.TrimSuffix(wrapped.String(), "\n") + " "
	}
	for attempt := 0; attempt < 3; attempt++ {
		answer, err := ask(prompt, "--check COMMAND")
		if err != nil {
			return "", false, err
		}
		switch {
		case answer == "":
			command = defaultEntry.Command
		case answer == "-":
			command = ""
		case strings.Trim(answer, "0123456789") == "":
			fmt.Fprint(w, initCheckTypedRetry)
			continue
		default:
			command = answer
		}
		initChangeLater(w, "edit default_check in .brigada/project.yaml, then run brigada project import")
		return command, false, nil
	}
	return "", true, nil
}
