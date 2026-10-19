package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/lifecycle"
	"github.com/Blackfire-AI/brigada/internal/term"
)

const (
	initCheckIntro = "\nStep 3 of 3: how brigada tests the work\n" +
		"  When an agent finishes a task, brigada runs one command in that task's\n" +
		"  own copy of the repo (never in your working copy):\n" +
		"    passes (exit 0)  the work goes on to review by another model family\n" +
		"    fails            the work goes back to the agent, with the output\n" +
		"  Use the command you run yourself to be sure nothing broke: tests,\n" +
		"  lint, type checks. Claude workers may run it too before handing in.\n"
	initCheckCurrent    = "current, from .brigada/project.yaml"
	initCheckTypedRetry = "  type a command, or - for no check; try again.\n"
)

// initCheckEntries is the menu: the project's current check first when it
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
// menu entry carries (the candidate's when the entry has none), else a scope
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

// askInitCheck prints Step 3 and asks for the check. It returns the command
// chosen; exhausted is true when three answers were all unusable.
func askInitCheck(w io.Writer, ask func(prompt, flags string) (string, error), entries []checkCandidate) (command string, exhausted bool, err error) {
	fmt.Fprintln(w)
	_, explanation, _ := strings.Cut(strings.TrimPrefix(initCheckIntro, "\n"), "\n")
	initStep(w, 4, "how brigada tests the work", explanation)
	numbered := len(entries) - 1 // The last entry is always no check, selected with "-".
	defaultIndex := initCheckDefault(entries)
	defaultAnswer := "-"
	if entries[defaultIndex].Command != "" {
		defaultAnswer = strconv.Itoa(defaultIndex + 1)
	}
	prompt := "  Type a command, or - for no check [-]: "
	if numbered == 0 {
		fmt.Fprint(w, "\n  Found in this repo: nothing brigada recognises.\n")
	} else {
		header := "Found in this repo:"
		if entries[defaultIndex].Note == initCheckCurrent {
			header = "Choices:"
		}
		fmt.Fprintf(w, "\n  %s\n", header)
		prompt = fmt.Sprintf("  Pick %s or -, or type a command [%s]: ", initPickRange(numbered), defaultAnswer)
	}
	width := term.VisibleWidth("no check")
	for _, e := range entries {
		width = max(width, term.VisibleWidth(e.Command))
	}
	for i, e := range entries {
		note, tags := initNoteTags(e.Note)
		if i == defaultIndex && tags == "" {
			tags = "default"
		}
		id, name := strconv.Itoa(i+1), e.Command
		if name == "" {
			id, name = "-", "no check"
			note = "work goes straight to review; add one later"
		}
		prefix := initMenuSymbolPrefix(w, "    ", id, name, width, i == defaultIndex) + "  "
		if initPresentation(w).styled {
			printInitRow(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), note, tags)
		} else {
			limit := initPresentation(w).style.Width() - 1
			room := limit - term.VisibleWidth(prefix) - term.VisibleWidth(tags)
			if note != "" && tags != "" {
				room -= 2 // initRowNote separates the note and tag with two spaces.
			}
			// Keep the no-check explanation complete, wrapping when needed.
			if e.Command == "" {
				printInitRow(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), note, tags)
				continue
			}
			note = truncateNote(note, room)
			row := prefix + initRowNote(w, note, tags)
			if term.VisibleWidth(row) > limit {
				// Long commands can leave no room even for the tag alone.
				printInitWrapped(w, "    ", "        ", strings.TrimSpace(row))
			} else {
				fmt.Fprintln(w, row)
			}
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		answer, err := ask(prompt, "--check COMMAND")
		if err != nil {
			return "", false, err
		}
		switch {
		case answer == "":
			command = entries[defaultIndex].Command
		case answer == "-":
			command = ""
		case strings.Trim(answer, "0123456789") == "":
			n, _ := strconv.Atoi(answer)
			if numbered == 0 {
				fmt.Fprint(w, initCheckTypedRetry)
				continue
			}
			if n < 1 || n > numbered {
				fmt.Fprintf(w, "  pick %s or - or type a command; try again.\n", initPickRange(numbered))
				continue
			}
			command = entries[n-1].Command
		default:
			command = answer
		}
		initChangeLater(w, "edit default_check in .brigada/project.yaml, then run brigada project import")
		return command, false, nil
	}
	return "", true, nil
}
