package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

// agentsStdin is where agents update reads its answers; tests inject them.
var agentsStdin io.Reader = os.Stdin

// agentsListed are the harnesses whose CLI lists its models cheaply, with
// the sources init uses. Kimi reads a config file and dsh has a static list:
// neither can offer a model init did not already show.
var agentsListed = []string{"claude", "codex", "opencode"}

// agentsCacheTTL is how long status and top trust the cached listing before
// status runs the CLIs again.
const agentsCacheTTL = 12 * time.Hour

const agentsCacheName = "models-offered.json"

// agentsOffer is what the CLIs list: harness/model pairs in listing order.
type agentsOffer struct {
	models []string
	// defaults maps a harness to the model its own default resolves to, so a
	// saved model=default line covers that model.
	defaults map[string]string
	// complete marks the harnesses whose list is whole enough to say a
	// registered model is gone. Claude's aliases are not.
	complete map[string]bool
}

// agentsDiff is an offer set against the agents file.
type agentsDiff struct {
	unregistered []string
	gone         []string
	// superseded maps a registered pair whose model a newer version of the
	// same line replaces, among what its CLI offers, to the newer pair.
	superseded map[string]string
}

// agentsCache is the listing kept between status and top calls.
type agentsCache struct {
	Checked  time.Time         `json:"checked"`
	Models   []string          `json:"models"`
	Defaults map[string]string `json:"defaults,omitempty"`
}

func agentsCachePath(rt *runtime) string {
	return filepath.Join(runnerStateDir(rt), agentsCacheName)
}

func (o agentsOffer) cache(now time.Time) agentsCache {
	return agentsCache{Checked: now.UTC(), Models: o.models, Defaults: o.defaults}
}

func (c agentsCache) offer() agentsOffer {
	return agentsOffer{models: c.Models, defaults: c.Defaults}
}

func readAgentsCache(rt *runtime) (agentsCache, bool) {
	data, err := os.ReadFile(agentsCachePath(rt))
	if err != nil {
		return agentsCache{}, false
	}
	var c agentsCache
	if json.Unmarshal(data, &c) != nil || c.Checked.IsZero() {
		return agentsCache{}, false
	}
	return c, true
}

// writeAgentsCache is best effort: a cache that cannot be written only means
// the next call lists again.
func writeAgentsCache(rt *runtime, c agentsCache) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	dir := runnerStateDir(rt)
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	f, err := os.CreateTemp(dir, ".models-offered-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return
	}
	if f.Close() != nil {
		return
	}
	_ = os.Rename(f.Name(), agentsCachePath(rt))
}

// listOfferedAgents runs the cheap model listings init uses. A CLI that is
// not installed offers nothing.
func listOfferedAgents(ctx context.Context) agentsOffer {
	offer := agentsOffer{defaults: map[string]string{}, complete: map[string]bool{}}
	add := func(harness, model string) {
		pair := harness + "/" + model
		if model != "" && model != "default" && !slices.Contains(offer.models, pair) {
			offer.models = append(offer.models, pair)
		}
	}
	for _, harness := range agentsListed {
		var source initModelSource
		switch harness {
		case "claude":
			source = initClaudeModels(ctx)
		case "codex":
			source = initCodexModels(ctx)
		case "opencode":
			source = initOpenCodeModels(ctx)
		}
		if !source.signIn.installed {
			continue
		}
		for _, entry := range source.entries {
			if entry.value != "default" {
				add(harness, entry.value)
				continue
			}
			if entry.name != "" {
				add(harness, entry.name)
				offer.defaults[harness] = entry.name
			}
		}
		// Models of unknown family are left out of the entries, so a list
		// that skipped any cannot say a registered model is gone.
		switch harness {
		case "codex":
			offer.complete[harness] = len(source.entries) > 1 && source.unknownCount == 0
		case "opencode":
			offer.complete[harness] = len(source.entries) > 0 && source.unknownCount == 0
		}
	}
	return offer
}

// registeredAgents parses the agents file's lines the way the runner does and
// leaves out every line it cannot read: comments and lines of a newer format
// stay in the file, but they register nothing here.
func registeredAgents(lines []string) []agentSpec {
	var specs []agentSpec
	for _, line := range lines {
		var one agentSpecs
		if one.Set(line) == nil {
			specs = append(specs, one.specs[0])
		}
	}
	return specs
}

// agentPair is the harness/model an agent line registers, with the model the
// runner would fill in.
func agentPair(spec agentSpec) string {
	model := spec.model
	if model == "" {
		model = runnerDefaultModel(spec.harness)
	}
	return spec.harness + "/" + model
}

func diffAgents(offer agentsOffer, registered []agentSpec) agentsDiff {
	have := map[string]bool{}
	for _, spec := range registered {
		have[agentPair(spec)] = true
	}
	superseded := supersededPairs(offer.models, registered)
	d := agentsDiff{superseded: map[string]string{}}
	for _, pair := range offer.models {
		harness, model, _ := strings.Cut(pair, "/")
		if have[pair] || (offer.defaults[harness] == model && have[harness+"/default"]) || superseded[pair] != "" {
			continue
		}
		d.unregistered = append(d.unregistered, pair)
	}
	for _, spec := range registered {
		pair := agentPair(spec)
		if strings.HasSuffix(pair, "/default") || slices.Contains(d.gone, pair) {
			continue
		}
		if offer.complete[spec.harness] && !slices.Contains(offer.models, pair) {
			d.gone = append(d.gone, pair)
			continue
		}
		if newer := superseded[pair]; newer != "" {
			d.superseded[pair] = newer
		}
	}
	return d
}

// supersededPairs maps each offered or registered harness/model pair that a
// newer version of the same line, offered by the same CLI or registered,
// replaces to the newest pair of that line (adapter.Superseded).
func supersededPairs(offered []string, registered []agentSpec) map[string]string {
	models := map[string][]string{}
	var harnesses []string
	add := func(pair string) {
		harness, model, _ := strings.Cut(pair, "/")
		if _, ok := models[harness]; !ok {
			harnesses = append(harnesses, harness)
		}
		if !slices.Contains(models[harness], model) {
			models[harness] = append(models[harness], model)
		}
	}
	for _, pair := range offered {
		add(pair)
	}
	for _, spec := range registered {
		add(agentPair(spec))
	}
	out := map[string]string{}
	for _, harness := range harnesses {
		for model, newer := range adapter.Superseded(models[harness]) {
			out[harness+"/"+model] = harness + "/" + newer
		}
	}
	return out
}

// agentsHint is the one to two lines status and top show when the CLIs offer
// models no agent registers or when a newer model version replaces registered
// ones, or "" when there is none to say. It never runs a CLI on its own: with
// refresh it lists again when the cache is missing or older than the TTL, and
// top, which redraws every tick, passes false and refreshes from the side.
func agentsHint(ctx context.Context, rt *runtime, refresh bool) string {
	lines, err := runnerstate.LoadAgents(runnerStateDir(rt))
	if err != nil || len(lines) == 0 {
		return ""
	}
	if refresh {
		refreshAgentsCacheIfStale(ctx, rt)
	}
	cache, ok := readAgentsCache(rt)
	if !ok {
		return ""
	}
	d := diffAgents(cache.offer(), registeredAgents(lines))
	var hint []string
	if n := len(d.unregistered); n > 0 {
		noun := "models"
		if n == 1 {
			noun = "model"
		}
		hint = append(hint, fmt.Sprintf("%d %s your CLIs offer but no agent registers: brigada agents", n, noun))
	}
	if n := len(d.superseded); n > 0 {
		runs := fmt.Sprintf("%d agents run", n)
		if n == 1 {
			runs = "1 agent runs"
		}
		hint = append(hint, runs+" a model a newer version replaces: brigada agents update")
	}
	return strings.Join(hint, "\n")
}

// refreshAgentsCacheIfStale lists the CLIs again when agents are saved and
// the cache is missing or older than the TTL; otherwise it does nothing.
func refreshAgentsCacheIfStale(ctx context.Context, rt *runtime) {
	if lines, err := runnerstate.LoadAgents(runnerStateDir(rt)); err != nil || len(lines) == 0 {
		return
	}
	if cache, ok := readAgentsCache(rt); ok && time.Since(cache.Checked) <= agentsCacheTTL {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	writeAgentsCache(rt, listOfferedAgents(ctx).cache(time.Now()))
}

// runAgents is `brigada agents`, `brigada agents add` and
// `brigada agents update`.
func runAgents(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return runAgentsAdd(ctx, args[1:], stdout, stderr)
		case "update":
			return runAgentsUpdate(ctx, args[1:], stdout, stderr)
		}
	}
	return runAgentsList(ctx, args, stdout, stderr)
}

// runAgentsList compares what the CLIs offer with the agents file and prints
// the difference; it changes nothing but the cached listing.
func runAgentsList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents",
		"brigada agents",
		"Show the models your CLIs offer that no agent registers, and registered models a CLI no longer lists; read-only. brigada agents add <harness>/<model> registers one.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "agents takes no arguments, got %q; to register a model run brigada agents add <harness>/<model>", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	lines, err := runnerstate.LoadAgents(runnerStateDir(rt))
	if err != nil {
		return fail(stderr, err)
	}
	if len(lines) == 0 {
		return fail(stderr, fmt.Errorf("no saved agents; run brigada init in a repository first"))
	}
	offer := listOfferedAgents(ctx)
	writeAgentsCache(rt, offer.cache(time.Now()))
	registered := registeredAgents(lines)
	printAgentsReport(stdout, filepath.Join(runnerStateDir(rt), "agents"), offer, registered, diffAgents(offer, registered), rt.file.Caps.TierOf)
	return 0
}

func printAgentsReport(w io.Writer, path string, offer agentsOffer, registered []agentSpec, d agentsDiff, tier func(harness, model string) string) {
	if len(offer.models) == 0 {
		fmt.Fprintln(w, "No model listing came back: none of the claude, codex or opencode CLIs listed models.")
	}
	if len(d.unregistered) == 0 {
		if len(offer.models) > 0 {
			fmt.Fprintln(w, "Every model your CLIs offer is registered.")
		}
	} else {
		fmt.Fprintf(w, "Offered by your CLIs, not registered (%d):\n", len(d.unregistered))
		width := 0
		for _, pair := range d.unregistered {
			width = max(width, len(pair))
		}
		for _, pair := range d.unregistered {
			harness, model, _ := strings.Cut(pair, "/")
			fmt.Fprintf(w, "  %-*s  tier %s, shares account %s\n", width, pair, tier(harness, model), agentsAccount(registered, harness))
		}
		fmt.Fprintln(w, "Register one with: brigada agents add <harness>/<model> [--classes ...] [--effort ...]")
		fmt.Fprintln(w, "Nothing is registered automatically: a new model can spend a paid account.")
		fmt.Fprintln(w, "A tier here comes from brigada's model table unless tier.<harness>/<model> in the config file sets it. Models a newer version of the same line replaces are left out.")
	}
	if len(d.superseded) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Registered, but a newer version of the model is listed or registered (%d):\n", len(d.superseded))
		pairs := slices.Sorted(maps.Keys(d.superseded))
		width := 0
		for _, pair := range pairs {
			width = max(width, len(pair))
		}
		for _, pair := range pairs {
			fmt.Fprintf(w, "  %-*s  replaced by %s\n", width, pair, d.superseded[pair])
		}
		fmt.Fprintf(w, "To retire one, remove its line from %s and restart the runner.\n", initHomePath(path))
		fmt.Fprintln(w, "To move one to the newer model, run brigada agents update: it rewrites each line you accept and restarts the runner.")
	}
	if len(d.gone) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Registered, but the CLI no longer lists the model (%d):\n", len(d.gone))
		for _, pair := range d.gone {
			fmt.Fprintf(w, "  %s\n", pair)
		}
		fmt.Fprintf(w, "If a model is gone, remove its line from %s and restart the runner.\n", initHomePath(path))
	}
}

// agentsAccount is the account a new agent of the harness would share: the
// one the harness's registered agents use, else the harness's own.
func agentsAccount(registered []agentSpec, harness string) string {
	for _, spec := range registered {
		if spec.harness == harness {
			return spec.accountLabel()
		}
	}
	return harness
}

// runAgentsAdd appends one agent line to the agents file. It validates
// the line the way the runner does at startup, then hands the running
// runner a restart request it honours between jobs — the new line is
// served by the fresh runner — or starts one when none runs.
func runAgentsAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents add",
		"brigada agents add [--classes CLASSES] [--effort LEVEL] [--now] <harness>/<model>",
		"Register one model as an agent: append its line to the agents file, keeping every other line, and restart the runner to serve it.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	classes := fs.String("classes", defaultClasses, "the task classes the agent takes, comma-separated, or none for a foreman-only agent")
	effort := fs.String("effort", "", "the agent's effort level, validated per harness (default: the CLI's own)")
	now := fs.Bool("now", false, "restart the runner at once, cutting its running jobs; without it the restart waits out running jobs")
	// The model and the flags may interleave; the flag package stops at the
	// first positional, so parse again after each one.
	var rest []string
	remaining := args
	for {
		if code, stop := common.parse(stdout, stderr, remaining); stop {
			return code
		}
		left := fs.Args()
		if len(left) == 0 {
			break
		}
		rest = append(rest, left[0])
		remaining = left[1:]
	}
	if len(rest) != 1 {
		return usagef(stderr, "usage: brigada agents add <harness>/<model> [--classes ...] [--effort ...]")
	}
	harness, model, ok := strings.Cut(rest[0], "/")
	if !ok || harness == "" || model == "" {
		return usagef(stderr, "agent must be <harness>/<model>, got %q", rest[0])
	}
	// A comma would end the model field of the line the runner parses.
	if strings.ContainsAny(model, ", \t\r\n") {
		return usagef(stderr, "model %q must not contain commas or whitespace", model)
	}
	// --classes given nowhere keeps the default, implement,review,answer.
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	spec := agentSpec{harness: harness, model: model, effort: *effort}
	if !explicit["classes"] {
		spec.classes = harnessDefaultClasses(harness)
	} else if *classes == "none" {
		spec.classes = []string{}
	} else if spec.classes = splitList(*classes); len(spec.classes) == 0 {
		return usagef(stderr, "--classes must name at least one class, or none")
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	dir := runnerStateDir(rt)
	lines, err := runnerstate.LoadAgents(dir)
	if err != nil {
		return fail(stderr, err)
	}
	if len(lines) == 0 {
		return fail(stderr, fmt.Errorf("no saved agents; run brigada init in a repository first"))
	}
	pair := agentPair(spec)
	registered := registeredAgents(lines)
	for _, have := range registered {
		if agentPair(have) == pair {
			return fail(stderr, fmt.Errorf("agent %s is already registered; edit its line in %s to change it", pair, initHomePath(filepath.Join(dir, "agents"))))
		}
	}
	// The report says a new model shares its harness's registered account,
	// so the line carries that account when it is not the harness name.
	if account := agentsAccount(registered, harness); account != harness {
		spec.account = account
	}
	// Read the line back the way the runner reads the file at startup, so
	// what is validated is what will be started.
	line := spec.String()
	var parsed agentSpecs
	if err := parsed.Set(line); err != nil {
		return usagef(stderr, "%v", err)
	}
	if _, adverts, err := resolveRunnerAgents(rt, parsed.specs, ""); err != nil {
		return usagef(stderr, "%v", err)
	} else if len(adverts) != 1 {
		return fail(stderr, fmt.Errorf("agent %s did not resolve to one agent", pair))
	}
	if err := runnerstate.AppendAgent(dir, line); err != nil {
		return fail(stderr, err)
	}
	classesText := strings.Join(spec.classes, ",")
	if len(spec.classes) == 0 {
		classesText = "none"
	}
	fmt.Fprintf(stdout, "registered %s (tier %s, classes %s)\n", pair, rt.file.Caps.TierOf(harness, model), classesText)
	fmt.Fprintf(stdout, "added to %s: %s\n", initHomePath(filepath.Join(dir, "agents")), line)
	return restartRunnerForAgents(ctx, rt, *now, stdout, stderr)
}

// restartRunnerForAgents is the shared tail of agents add and agents
// update: a running runner is handed a restart request it honours between
// jobs, so no running job is cut — unless now asked for exactly that.
// Without one there is nothing to restart; the changed lines serve from
// the next start.
func restartRunnerForAgents(ctx context.Context, rt *runtime, now bool, stdout, stderr io.Writer) int {
	dir := runnerStateDir(rt)
	if _, running, err := runnerstate.Running(dir); err != nil {
		return fail(stderr, err)
	} else if !running {
		fmt.Fprintln(stdout, "No runner running; brigada start serves it.")
		return 0
	}
	saved, err := runnerstate.LoadAgents(dir)
	if err != nil {
		return fail(stderr, err)
	}
	// The runner takes the lines it parses, as it does at its own
	// startup; a comment or a line of a newer format stays in the file
	// and is simply not offered.
	var specs []string
	for _, line := range saved {
		var one agentSpecs
		if one.Set(line) == nil {
			specs = append(specs, line)
		}
	}
	if len(specs) == 0 {
		return fail(stderr, fmt.Errorf("no agent the runner can serve; run brigada init in a repository first"))
	}
	parallel, err := runnerstate.LoadParallel(dir)
	if err != nil {
		return fail(stderr, err)
	}
	mode := runnerstate.RestartDrain
	if now {
		mode = runnerstate.RestartNow
	}
	if !now {
		fmt.Fprintln(stdout, "runner restarting: it finishes its running jobs first; --now cuts them")
	}
	result, err := startDetachedRunner(ctx, rt, specs, parallel, mode)
	if err != nil {
		return fail(stderr, err)
	}
	if result.Existing {
		fmt.Fprintf(stdout, "runner already running (pid %d); the new line serves after its next restart\n", result.Record.PID)
	} else {
		fmt.Fprintf(stdout, "runner restarted (pid %d)\n", result.Record.PID)
	}
	return 0
}

// agentsUpdateCandidate is one superseded pair's rewrite: the raw line it
// replaces, the line replacing it, and the saved effort the rewrite had to
// drop when the new model refused it.
type agentsUpdateCandidate struct {
	oldPair, newPair string
	oldLine, newLine string
	effortDropped    string
}

// runAgentsUpdate is `brigada agents update`: every registered agent whose
// model a newer version of the same line replaces is rewritten to the newer
// model in place, each rewrite accepted interactively or by --yes, and a
// running runner restarts to serve the new lines.
func runAgentsUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents update",
		"brigada agents update [--yes] [--dry-run] [--now]",
		"Move registered agents to the newer model of their line the CLIs list: rewrite each superseded line of the agents file in place, keeping every other line, and restart the runner to serve the new models.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	yes := fs.Bool("yes", false, "apply every update without prompting")
	dryRun := fs.Bool("dry-run", false, "print the rewrites and exit: nothing is written and the runner is not restarted")
	now := fs.Bool("now", false, "restart the runner at once, cutting its running jobs; without it the restart waits out running jobs")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "agents update takes no arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	dir := runnerStateDir(rt)
	lines, err := runnerstate.LoadAgents(dir)
	if err != nil {
		return fail(stderr, err)
	}
	if len(lines) == 0 {
		return fail(stderr, fmt.Errorf("no saved agents; run brigada init in a repository first"))
	}
	offer := listOfferedAgents(ctx)
	writeAgentsCache(rt, offer.cache(time.Now()))
	registered := registeredAgents(lines)
	d := diffAgents(offer, registered)
	if len(d.superseded) == 0 {
		fmt.Fprintln(stdout, "No agent runs a model a newer version replaces.")
		return 0
	}
	// Every candidate is built and validated before anything is printed or
	// written: one refusal fails the command with the file untouched. stay
	// holds the pairs the file registers once the candidates so far are
	// applied, so a rewrite never duplicates a line that stays. claimed
	// holds the pairs accepted candidates so far claim, each mapped to the
	// old pair that claimed it, so a collision between two candidates is
	// told apart from a target the file itself registers.
	stay := map[string]bool{}
	claimed := map[string]string{}
	for _, spec := range registered {
		stay[agentPair(spec)] = true
	}
	var candidates []agentsUpdateCandidate
	for _, oldPair := range slices.Sorted(maps.Keys(d.superseded)) {
		newPair := d.superseded[oldPair]
		// A line the runner can no longer parse registers nothing here and
		// is left alone.
		var oldLine string
		var spec agentSpec
		for _, line := range lines {
			var one agentSpecs
			if one.Set(line) == nil && agentPair(one.specs[0]) == oldPair {
				oldLine, spec = line, one.specs[0]
				break
			}
		}
		if oldLine == "" {
			continue
		}
		if first, done := claimed[newPair]; done {
			return fail(stderr, fmt.Errorf("%s and %s both update to %s; remove one line and run again", first, oldPair, newPair))
		}
		if stay[newPair] {
			return fail(stderr, fmt.Errorf("agent %s is already registered; edit its line in %s to change it", newPair, initHomePath(filepath.Join(dir, "agents"))))
		}
		// The model is the pair's part after the first slash, so an
		// opencode provider/model keeps its own slash.
		_, model, _ := strings.Cut(newPair, "/")
		spec.model = model
		// The rewritten line is read back and resolved the way agents add
		// validates its line, so what is written is what the runner serves.
		validate := func(line string) error {
			var parsed agentSpecs
			if err := parsed.Set(line); err != nil {
				return err
			}
			if _, adverts, err := resolveRunnerAgents(rt, parsed.specs, ""); err != nil {
				return err
			} else if len(adverts) != 1 {
				return fmt.Errorf("agent %s did not resolve to one agent", newPair)
			}
			return nil
		}
		c := agentsUpdateCandidate{oldPair: oldPair, newPair: newPair, oldLine: oldLine, newLine: spec.String()}
		if err := validate(c.newLine); err != nil {
			if spec.effort == "" {
				return fail(stderr, err)
			}
			// A saved effort the new model refuses falls back to the CLI's
			// own default; any other refusal fails the command.
			effort := spec.effort
			spec.effort = ""
			without := spec.String()
			if err := validate(without); err != nil {
				return fail(stderr, err)
			}
			c.newLine, c.effortDropped = without, effort
		}
		delete(stay, oldPair)
		stay[newPair] = true
		claimed[newPair] = oldPair
		candidates = append(candidates, c)
	}
	printCandidate := func(c agentsUpdateCandidate) {
		fmt.Fprintf(stdout, "%s → %s\n  %s\n", c.oldPair, c.newPair, c.newLine)
		if c.effortDropped != "" {
			fmt.Fprintf(stdout, "  note: effort=%s is not accepted for %s; the line leaves the effort to the CLI's default\n", c.effortDropped, c.newPair)
		}
	}
	if *dryRun {
		for _, c := range candidates {
			printCandidate(c)
		}
		fmt.Fprintln(stdout, "Dry run: nothing written, nothing restarted.")
		return 0
	}
	input := bufio.NewReader(agentsStdin)
	acceptAll := *yes
	var accepted []agentsUpdateCandidate
	for _, c := range candidates {
		printCandidate(c)
		if !acceptAll {
			fmt.Fprint(stdout, "Apply this update? y = yes, n = no (the default), a = yes to all: ")
			answer, err := input.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return fail(stderr, err)
			}
			fmt.Fprintln(stdout)
			switch strings.ToLower(strings.TrimSpace(answer)) {
			case "y", "yes":
			case "a", "all":
				acceptAll = true
			default:
				continue
			}
		}
		accepted = append(accepted, c)
	}
	if len(accepted) == 0 {
		fmt.Fprintln(stdout, "Nothing updated.")
		return 0
	}
	for _, c := range accepted {
		if err := runnerstate.ReplaceAgent(dir, c.oldLine, c.newLine); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "updated %s → %s", c.oldPair, c.newPair)
		if c.effortDropped != "" {
			fmt.Fprintf(stdout, " (effort=%s refused; the CLI's default applies)", c.effortDropped)
		}
		fmt.Fprintln(stdout)
	}
	fmt.Fprintf(stdout, "rewritten in %s\n", initHomePath(filepath.Join(dir, "agents")))
	return restartRunnerForAgents(ctx, rt, *now, stdout, stderr)
}
