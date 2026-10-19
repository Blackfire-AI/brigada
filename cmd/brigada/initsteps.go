package main

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/term"
)

// printInitSignedIn opens init's questions: the CLIs known to be signed in,
// then one line for each installed CLI whose sign-in init cannot check.
func printInitSignedIn(w io.Writer, probes map[string]harnessProbe, flagged map[string]bool) {
	var signedIn, unchecked []string
	for _, h := range initHarnesses {
		p := probes[h]
		switch {
		case !p.installed:
		case p.signedIn || flagged[h]:
			name := h
			if detail := initSignInDetail(p.status); p.signedIn && detail != "" {
				name += " (" + detail + ")"
			}
			signedIn = append(signedIn, name)
		case !p.checked:
			unchecked = append(unchecked, h)
		}
	}
	if len(signedIn) > 0 {
		printInitWrapped(w, "", "  ", "Signed in: "+strings.Join(signedIn, ", ")+".")
	}
	for _, h := range unchecked {
		fmt.Fprintf(w, "%s: init can't check sign-in.\n", h)
	}
}

// initSignInDetail turns "signed in to 2 providers (opencode auth list)" into
// "2 providers, opencode auth list"; a plain "signed in" has no detail.
func initSignInDetail(status string) string {
	rest, ok := strings.CutPrefix(status, "signed in to ")
	if !ok {
		return ""
	}
	what, command, ok := strings.Cut(rest, " (")
	if !ok {
		return what
	}
	return what + ", " + strings.TrimSuffix(command, ")")
}

// parseInitForeman reads harness/model[@effort], as --foreman takes it and as
// the foreman is saved.
func parseInitForeman(value string) (spec agentSpec, hasEffort, ok bool) {
	pair, effort := value, ""
	if i := strings.LastIndex(value, "@"); i >= 0 {
		pair, effort, hasEffort = value[:i], value[i+1:], true
	}
	harness, model, found := strings.Cut(pair, "/")
	if !found || harness == "" || model == "" || (hasEffort && effort == "") {
		return agentSpec{}, false, false
	}
	return agentSpec{harness: harness, model: model, effort: effort}, hasEffort, true
}

// initEligibleEntries are a CLI's models whose family init can tell.
func initEligibleEntries(harness string, source initModelSource) []initModelEntry {
	var entries []initModelEntry
	for _, entry := range source.entries {
		if _, err := initModelFamily(harness, entry.value); err == nil {
			entries = append(entries, entry)
		}
	}
	return entries
}

// initUseNames lists a CLI's models for the question that offers them: by
// tier, heaviest first, when there are at most three; else the first and a count.
func initUseNames(harness string, entries []initModelEntry, tier func(harness, model string) string) string {
	if len(entries) <= 3 {
		rank := func(entry initModelEntry) int {
			return slices.Index([]string{adapter.TierHeavy, adapter.TierStandard, adapter.TierLight}, tier(harness, entry.value))
		}
		sorted := slices.Clone(entries)
		slices.SortStableFunc(sorted, func(a, b initModelEntry) int { return rank(a) - rank(b) })
		names := make([]string, len(sorted))
		for i, entry := range sorted {
			names[i] = initMenuModelName(entry)
		}
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", initMenuModelName(entries[0]), len(entries)-1)
}

// initForemanChoice is one row of the foreman menu: a model of a signed-in
// claude or codex CLI.
type initForemanChoice struct {
	harness string
	entry   initModelEntry
}

func (c initForemanChoice) name() string {
	return c.harness + "/" + initMenuModelName(c.entry)
}

func (c initForemanChoice) matches(harness, model string) bool {
	return c.harness == harness && (c.entry.value == model || initMenuModelName(c.entry) == model)
}

// initForemanCandidates lists every model of the signed-in claude and codex
// CLIs, claude first: the only CLIs that resume a session can plan.
func initForemanCandidates(sources map[string]initModelSource, signedIn func(string) bool) []initForemanChoice {
	var candidates []initForemanChoice
	for _, h := range initHarnesses {
		if !adapter.ResumeCapable(h) || !sources[h].signIn.installed || !signedIn(h) {
			continue
		}
		for _, entry := range initEligibleEntries(h, sources[h]) {
			candidates = append(candidates, initForemanChoice{harness: h, entry: entry})
		}
	}
	return candidates
}

// initResolveForeman finds a typed or flagged harness/model among the
// candidates, or accepts a model of a signed-in CLI whose family init can tell.
func initResolveForeman(candidates []initForemanChoice, sources map[string]initModelSource, harness, model string) (initForemanChoice, error) {
	for _, c := range candidates {
		if c.matches(harness, model) {
			return c, nil
		}
	}
	if !slices.ContainsFunc(candidates, func(c initForemanChoice) bool { return c.harness == harness }) {
		return initForemanChoice{}, fmt.Errorf("%s/%s is not a model of a signed-in claude or codex", harness, model)
	}
	if _, err := initModelFamily(harness, model); err != nil {
		return initForemanChoice{}, err
	}
	return initForemanChoice{harness: harness, entry: initPickedModel(harness, model, sources)}, nil
}

// printInitForemanMenu prints the foreman's numbered models. The row for the
// default model is selected, and the saved foreman's is marked current.
func printInitForemanMenu(w io.Writer, candidates []initForemanChoice, defaultChoice int, saved agentSpec) {
	width := 0
	for _, c := range candidates {
		width = max(width, term.VisibleWidth(c.name()))
	}
	for i, c := range candidates {
		note, tags := c.entry.note, ""
		if c.entry.isDefault {
			if c.harness == "codex" {
				note = "your codex default"
				if path := codexConfigPath(); path != "" {
					note += " (" + initHomePath(path) + ")"
				}
			} else {
				tags = "your " + c.harness + " default"
			}
		}
		if saved.harness != "" && c.matches(saved.harness, saved.model) {
			tags = "current"
		}
		prefix := initMenuPrefix(w, "     ", i+1, c.name(), width, i+1 == defaultChoice)
		if note == "" && tags == "" {
			prefix = strings.TrimRight(prefix, " ")
		} else {
			prefix += "  "
		}
		printInitRow(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), note, tags)
	}
}

// initPickEffort asks how hard the foreman thinks. The model's own default
// level comes first, the rest in source order; Enter picks the first, or the
// saved level on a rerun, and records that level. A model with fewer than two
// levels is not asked.
func initPickEffort(w io.Writer, harness string, entry initModelEntry,
	ask func(string, string) (string, error), saved ...string) (string, error) {
	if !adapter.ResumeCapable(harness) {
		return "", nil // only a foreman is asked how hard to think
	}
	levels := slices.Clone(entry.effortLevels)
	first, note, tag := "", "", ""
	switch {
	case harness == "claude":
		first, note = "medium", "Claude Code's default"
	case entry.defaultEffort != "":
		first, tag = entry.defaultEffort, harness+"'s default"
		if entry.effortFromUser {
			tag = "your " + harness + " default"
		}
	}
	if i := slices.Index(levels, first); i >= 0 {
		levels = append([]string{first}, slices.Delete(levels, i, i+1)...)
	} else {
		note, tag = "", ""
	}
	if len(levels) < 2 {
		if len(saved) > 0 {
			return saved[0], nil
		}
		return "", nil
	}
	pickIndex, hasSaved := 0, false
	if len(saved) > 0 && saved[0] != "" {
		if i := slices.Index(levels, saved[0]); i >= 0 {
			pickIndex, hasSaved = i, true
		}
	}
	printInitWrapped(w, "    ", "    ", initDim(w, "How hard should the foreman think? Higher means better plans and decisions, and slower turns that use more of the plan's limits."))
	width := 0
	for _, level := range levels {
		width = max(width, term.VisibleWidth(level))
	}
	for i, level := range levels {
		rowNote, rowTag := "", ""
		if i == 0 {
			rowNote, rowTag = note, tag
		}
		if hasSaved && i == pickIndex {
			rowTag = "current"
		}
		prefix := initMenuPrefix(w, "     ", i+1, level, width, i == pickIndex)
		if rowNote == "" && rowTag == "" {
			prefix = strings.TrimRight(prefix, " ")
		} else {
			prefix += "  "
		}
		printInitRow(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), rowNote, rowTag)
	}
	flags := "--foreman HARNESS/MODEL@EFFORT"
	for attempt := 0; attempt < 3; attempt++ {
		answer, err := ask(fmt.Sprintf("    Pick %s or type a level [%d]: ", initPickRange(len(levels)), pickIndex+1), flags)
		if err != nil {
			return "", err
		}
		if answer == "" {
			return levels[pickIndex], nil
		}
		index := slices.Index(levels, answer)
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(levels) {
			index = n - 1
		}
		if index >= 0 {
			return levels[index], nil
		}
		if attempt < 2 {
			fmt.Fprintf(w, "    pick %s or a listed effort level; try again.\n", initPickRange(len(levels)))
		}
	}
	return "", fmt.Errorf("%s: pick %s or a listed effort level", flags, initPickRange(len(levels)))
}

// printInitOpenCodeMenu lists OpenCode's models, whose list spans providers.
// Saved picks are marked current and selected; without any, "don't use" is.
func printInitOpenCodeMenu(w io.Writer, entries []initModelEntry, unknown int, rerun bool) {
	out := initPresentation(w)
	style := out.style
	printInitWrapped(w, "    ", "    ", initDim(w, "Which models should opencode run? Its list spans providers, so pick the ones you pay for."))
	width := 0
	for _, entry := range entries {
		width = max(width, term.VisibleWidth(initMenuModelName(entry)))
	}
	for _, entry := range entries {
		prefix := initMenuPrefix(w, "     ", entry.id, initMenuModelName(entry), width, entry.isCurrent)
		tags := ""
		if entry.isCurrent {
			tags = "current"
			prefix += "  "
		} else {
			prefix = strings.TrimRight(prefix, " ")
		}
		printInitRow(w, prefix, strings.Repeat(" ", term.VisibleWidth(prefix)), "", tags)
	}
	indent, name := "     ", initDim(w, "  don't use opencode")
	if out.styled && !rerun {
		indent, name = "    "+style.Pointer(), "  "+style.Bold("don't use opencode")
	}
	fmt.Fprintln(w, indent+style.Accent(" -")+name)
	if unknown > 0 {
		printInitWrapped(w, "    ", "    ", initDim(w, fmt.Sprintf("%d more from `opencode models` aren't listed: brigada can't tell their model family, which it needs to pick reviewers.", unknown)))
	}
}

// initCurrentIDs is the saved picks as the menu's default answer: "1,3".
func initCurrentIDs(entries []initModelEntry) string {
	var ids []string
	for _, entry := range entries {
		if entry.isCurrent {
			ids = append(ids, strconv.Itoa(entry.id))
		}
	}
	return strings.Join(ids, ",")
}

// parseInitOpenCodePicks reads OpenCode's answer: "-" for none, or numbers
// from the menu and typed provider/model names, comma-separated.
func parseInitOpenCodePicks(entries []initModelEntry, answer string) ([]string, error) {
	var models []string
	for _, token := range splitList(answer) {
		if token == "-" {
			continue
		}
		model := token
		if n, err := strconv.Atoi(token); err == nil {
			index := slices.IndexFunc(entries, func(e initModelEntry) bool { return e.id == n })
			if index < 0 {
				return nil, fmt.Errorf("no entry numbered %d", n)
			}
			model = entries[index].value
		} else if _, err := initModelFamily("opencode", token); err != nil {
			return nil, err
		}
		if !slices.Contains(models, model) {
			models = append(models, model)
		}
	}
	return models, nil
}
