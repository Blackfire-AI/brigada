package main

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Blackfire-AI/brigada/internal/logo"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// initDetectCaps is a seam for transcript tests. Detect the original output
// stream, before wrapping it: buffers and pipes must retain plain output.
var initDetectCaps = logo.Detect

type initOutput struct {
	io.Writer
	style  term.Style
	styled bool
	utf8   bool
}

// Keep init's banner in the terminal's default foreground. The general logo
// renderer uses orange, which init's presentation deliberately avoids.
func printInitBanner(w io.Writer) {
	caps := detectCaps(w)
	if !caps.TTY {
		return
	}
	caps.Color = logo.NoColor
	if banner := logo.String(logo.Banner, caps); banner != "" {
		fmt.Fprint(w, banner+"\n")
	}
}

func initPresentation(w io.Writer) initOutput {
	if out, ok := w.(initOutput); ok {
		return out
	}
	return initOutput{Writer: w}
}

func initSeparator(w io.Writer) string {
	if initPresentation(w).utf8 {
		return " · "
	}
	return " - "
}

func initStep(w io.Writer, number int, title, explanation string) {
	out := initPresentation(w)
	if out.styled {
		fmt.Fprintln(w, out.style.Bold(fmt.Sprintf("%s Step %d of 4%s%s", out.style.Rule(), number, initSeparator(w), title)))
		fmt.Fprint(w, initDim(w, explanation))
	} else {
		fmt.Fprintf(w, "Step %d of 4: %s\n%s", number, title, explanation)
	}
}

func initDim(w io.Writer, text string) string {
	style := initPresentation(w).style
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = style.Dim(line)
		}
	}
	return strings.Join(lines, "\n")
}

var initPromptDefault = regexp.MustCompile(`\[[^\[\]]+\]`)

func initPrompt(w io.Writer, prompt string) string {
	out := initPresentation(w)
	if !out.styled {
		return prompt
	}
	// Keep the default's cyan independent of the surrounding bold prompt.
	var b strings.Builder
	bold := func(text string) {
		if text != "" {
			b.WriteString(out.style.Bold(text))
		}
	}
	start := 0
	for _, loc := range initPromptDefault.FindAllStringIndex(prompt, -1) {
		bold(prompt[start:loc[0]])
		b.WriteString(out.style.Accent(prompt[loc[0]:loc[1]]))
		start = loc[1]
	}
	bold(prompt[start:])
	return b.String()
}

func initMenuPrefix(w io.Writer, indent string, id int, name string, width int, selected bool) string {
	return initMenuSymbolPrefix(w, indent, strconv.Itoa(id), name, width, selected)
}

func initMenuSymbolPrefix(w io.Writer, indent, id, name string, width int, selected bool) string {
	out := initPresentation(w)
	if !out.styled {
		return fmt.Sprintf("%s%2s  %-*s", indent, id, width, name)
	}
	if selected {
		// Leave one space between the pointer and the first digit.
		number := fmt.Sprintf("%2s", id)
		column := len(indent) + len(number) - len(strings.TrimLeft(number, " ")) - 2
		indent = indent[:column] + out.style.Pointer() + indent[column+1:]
		name = out.style.Bold(name)
	}
	return indent + out.style.Accent(fmt.Sprintf("%2s", id)) + "  " + name + strings.Repeat(" ", max(0, width-term.VisibleWidth(name)))
}

// Check candidates mark the saved command in their note. Render that marker
// as a separate tag, just like model and effort choices.
func initNoteTags(note string) (string, string) {
	if note == "current" {
		return "", "current"
	}
	if rest, ok := strings.CutPrefix(note, "current, "); ok {
		return rest, "current"
	}
	return note, ""
}

func initRowNote(w io.Writer, note, tags string) string {
	out := initPresentation(w)
	text := ""
	if note != "" {
		text = out.style.Dim(note)
	}
	if tags != "" {
		if text != "" {
			text += "  "
		}
		text += out.style.Accent(tags)
	}
	return text
}

// Keep a multiword tag together and preserve the two spaces before it when
// wrapping the note. Ordinary prose wrapping normalizes spaces between words.
func printInitRow(w io.Writer, prefix, continuation, note, tag string) {
	var words []initWord
	for _, word := range strings.Fields(initDim(w, note)) {
		words = append(words, initWord{" ", word})
	}
	if tag != "" {
		words = append(words, initWord{"  ", initPresentation(w).style.Accent(tag)})
	}
	printInitWords(w, prefix, continuation, words)
}

func initAgentChangeLater(sources map[string]initModelSource) string {
	how := "run brigada init again"
	var paths []string
	for _, harness := range []string{"codex", "kimi"} {
		if !sources[harness].signIn.installed {
			continue
		}
		path := codexConfigPath()
		if harness == "kimi" {
			path = kimiConfigPath()
		}
		if path != "" {
			paths = append(paths, initHomePath(path))
		}
	}
	if len(paths) > 0 {
		label := "your own default: "
		if len(paths) > 1 {
			label = "your own defaults: "
		}
		how += "\n" + label + strings.Join(paths, ", ")
	}
	return how
}

func initChangeLater(w io.Writer, how string) {
	out := initPresentation(w)
	fmt.Fprintln(w)
	prefix := "  " + out.style.Dim(out.style.Again()+" ")
	continuation := strings.Repeat(" ", term.VisibleWidth(prefix))
	for _, line := range strings.Split("change later: "+how, "\n") {
		printInitWrapped(w, prefix, continuation, out.style.Dim(line))
		prefix = continuation
	}
}

func initCLIStatus(w io.Writer, harness string, probe harnessProbe, confirmed bool) {
	out := initPresentation(w)
	status := out.style.Bad(probe.status)
	switch {
	case confirmed:
		status = out.style.OK(out.style.Check() + " signed in")
	case !probe.checked:
		command := harness + " login"
		if harness == "opencode" {
			command = "opencode auth login"
		}
		status = out.style.Warn(out.style.Question() + " sign-in can't be checked")
		if harness != "dsh" {
			status += out.style.Dim(initSeparator(w)+"sign in with ") + out.style.Accent(command)
		}
	case harness == "opencode" && strings.HasPrefix(probe.status, "no stored provider credentials"):
		status = out.style.Warn(probe.status)
	}
	if harness == "dsh" {
		status = out.style.Dim("experimental"+initSeparator(w)) + status
	}
	printInitWrapped(w, "  "+out.style.Bold(harness)+"  ", "    ", status)
}

// Split long identifiers by visible width without cutting an SGR sequence.
func initSplitVisible(text string, width int) (string, string) {
	columns := 0
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "\x1b[") {
			end := i + 2
			for end < len(text) && (text[end] < 0x40 || text[end] > 0x7e) {
				end++
			}
			if end < len(text) {
				i = end + 1
				continue
			}
		}
		if columns == width {
			return text[:i], text[i:]
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
		columns++
	}
	return text, ""
}
