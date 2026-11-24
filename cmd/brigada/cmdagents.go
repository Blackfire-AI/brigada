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
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/runnerstate"
)

// agentsStdin is where agents update reads its answers; tests inject them.
var agentsStdin io.Reader = os.Stdin

// agentsListed are the harnesses whose models the listing sources init uses
// cover — all of init's, in its order: claude, codex and opencode list through
// their CLIs, kimi through its config tables, qwen-code through
// `qwen --list-models`, and dsh through a static list.
var agentsListed = initHarnesses

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
		case "kimi":
			source = initKimiModels(ctx)
		case "opencode":
			source = initOpenCodeModels(ctx)
		case "dsh":
			source = initDshModels(ctx)
		case "qwen-code":
			source = initQwenModels(ctx)
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
		// that skipped any cannot say a registered model is gone. Kimi's
		// config tables and dsh's static list are not the CLI's whole catalog,
		// so neither says a model is gone either.
		switch harness {
		case "codex":
			offer.complete[harness] = len(source.entries) > 1 && source.unknownCount == 0
		case "opencode":
			offer.complete[harness] = len(source.entries) > 0 && source.unknownCount == 0
		case "qwen-code":
			offer.complete[harness] = source.listed && len(source.entries) > 0 && source.unknownCount == 0
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
		hint = append(hint, fmt.Sprintf("%d %s your harnesses offer but no agent registers: brigada agents", n, noun))
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

// runAgents is `brigada agents`, `brigada agents add`, `brigada agents
// remove` and `brigada agents update`.
func runAgents(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return runAgentsAdd(ctx, args[1:], stdout, stderr)
		case "remove":
			return runAgentsRemove(ctx, args[1:], stdout, stderr)
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
		"Show the models your harnesses offer that no agent registers, and registered models a CLI no longer lists; read-only. brigada agents add <harness>/<model> registers one.")
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
	printAgentsReport(stdout, offer, registered, diffAgents(offer, registered), rt.file.Caps.TierOf)
	return 0
}

func printAgentsReport(w io.Writer, offer agentsOffer, registered []agentSpec, d agentsDiff, tier func(harness, model string) string) {
	if len(offer.models) == 0 {
		fmt.Fprintln(w, "No model listing came back: no harness offered models.")
		// Registered agents whose models supersede each other are still
		// worth naming without a listing.
		if len(d.superseded) > 0 {
			pairs := slices.Sorted(maps.Keys(d.superseded))
			for _, pair := range pairs {
				fmt.Fprintf(w, "  %s  superseded  (replaced by %s)\n", pair, d.superseded[pair])
			}
		}
	} else {
		// Every pair the harnesses offer, listed whole, each marked by
		// registration: the roster a removal or an add names its pairs
		// from. A pair whose model a newer version of the same line
		// replaces — registered or not — shows as superseded instead of
		// registered or not registered, with the newer pair named; the
		// coverage clauses are diffAgents' own, so a registered default
		// covers the model the CLI lists as its default.
		fmt.Fprintf(w, "Offered by your harnesses (%d):\n", len(offer.models))
		type agentsRow struct {
			pair, status, replacedBy string
		}
		superseded := supersededPairs(offer.models, registered)
		have := map[string]bool{}
		for _, spec := range registered {
			have[agentPair(spec)] = true
		}
		var rows []agentsRow
		for _, pair := range offer.models {
			r := agentsRow{pair: pair, status: "not registered"}
			if newer := superseded[pair]; newer != "" {
				r.status, r.replacedBy = "superseded", newer
			} else {
				harness, model, _ := strings.Cut(pair, "/")
				if have[pair] || (offer.defaults[harness] == model && have[harness+"/default"]) {
					r.status = "registered"
				}
			}
			rows = append(rows, r)
		}
		pairW, statusW := 0, 0
		for _, r := range rows {
			pairW = max(pairW, len(r.pair))
			statusW = max(statusW, len(r.status))
		}
		for _, r := range rows {
			harness, model, _ := strings.Cut(r.pair, "/")
			fmt.Fprintf(w, "  %-*s  %-*s  tier %s, shares account %s", pairW, r.pair, statusW, r.status, tier(harness, model), agentsAccount(registered, harness))
			if r.replacedBy != "" {
				fmt.Fprintf(w, "  (replaced by %s)", r.replacedBy)
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "Register one with: brigada agents add <harness>/<model> [--classes ...] [--effort ...]")
		fmt.Fprintln(w, "Nothing is registered automatically: a new model can spend a paid account.")
		fmt.Fprintln(w, "A tier here comes from brigada's model table unless tier.<harness>/<model> in the config file sets it.")
		for _, r := range rows {
			if r.replacedBy != "" {
				fmt.Fprintln(w, "A superseded model is one a newer version of the same line replaces: brigada agents update moves a registered one to the newer model; brigada agents remove retires one.")
				break
			}
		}
	}
	if len(d.gone) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Registered, but the CLI no longer lists the model (%d):\n", len(d.gone))
		for _, pair := range d.gone {
			fmt.Fprintf(w, "  %s\n", pair)
		}
		fmt.Fprintf(w, "If a model is gone, run brigada agents remove <harness>/<model>.\n")
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

// runAgentsRemove deletes every agent line that registers the named pair
// from the agents file. It refuses a pair no line registers and a removal
// that would leave the runner nothing to serve — both with the file
// untouched — then hands the running runner a restart request it honours
// between jobs, so the fresh runner no longer serves the agent, or notes
// the next start when none runs. Foremen are the engine's knowledge, so
// it also asks the engine which projects have their foremen on the pair
// and names each, with the switch to a pair an agent still offers.
func runAgentsRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents remove",
		"brigada agents remove [--now] <harness>/<model>",
		"Remove one agent: delete every line that registers it from the agents file, keeping every other line, and restart the runner to stop serving it.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
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
		return usagef(stderr, "usage: brigada agents remove <harness>/<model>")
	}
	harness, model, ok := strings.Cut(rest[0], "/")
	if !ok || harness == "" || model == "" {
		return usagef(stderr, "agent must be <harness>/<model>, got %q", rest[0])
	}
	// A comma would end the model field of the line the runner parses.
	if strings.ContainsAny(model, ", \t\r\n") {
		return usagef(stderr, "model %q must not contain commas or whitespace", model)
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
	// The pair matches every line that registers it, each read the way the
	// runner reads the file at startup; a comment or a line of a newer
	// format stays in the file and registers nothing here. left holds the
	// agents the removal would leave, so the runner is never left with
	// nothing to serve, and is the roster a foreman on the pair can be
	// switched to.
	pair := harness + "/" + model
	var matching []string
	var saved []string
	var left []agentSpec
	for _, line := range lines {
		var one agentSpecs
		if one.Set(line) != nil {
			continue
		}
		have := agentPair(one.specs[0])
		saved = append(saved, have)
		if have == pair {
			if !slices.Contains(matching, line) {
				matching = append(matching, line)
			}
			continue
		}
		left = append(left, one.specs[0])
	}
	if len(matching) == 0 {
		return fail(stderr, fmt.Errorf("agent %s is not registered; %s", pair, notRegisteredHint(pair, saved)))
	}
	if len(left) == 0 {
		return fail(stderr, fmt.Errorf("removing %s would leave no agent for the runner to serve; add another agent first with brigada agents add <harness>/<model>", pair))
	}
	// Every line is out of the file before anything says so; a failure
	// mid-loop leaves nothing claimed that did not happen.
	copies := make([]int, len(matching))
	for i, line := range matching {
		n, err := runnerstate.RemoveAgent(dir, line)
		if err != nil {
			return fail(stderr, err)
		}
		copies[i] = n
	}
	fmt.Fprintf(stdout, "removed %s\n", pair)
	path := initHomePath(filepath.Join(dir, "agents"))
	for i, line := range matching {
		if copies[i] > 1 {
			fmt.Fprintf(stdout, "removed %d copies from %s: %s\n", copies[i], path, line)
			continue
		}
		fmt.Fprintf(stdout, "removed from %s: %s\n", path, line)
	}
	printRemovedForemen(ctx, rt, stdout, stderr, pair, harness, left)
	return restartRunnerForAgents(ctx, rt, *now, stdout, stderr)
}

// connectAgents is how agents remove and agents update reach the engine;
// tests replace it.
var connectAgents = connect

// agentsForeman is a project whose foreman is on a pair a removal takes
// from the roster or a rewrite moves: the repo URL as the engine lists it,
// the form --repo takes back, and the foreman's saved effort — "" when
// none is set or none could be read.
type agentsForeman struct {
	repo, effort string
}

// engineForemen is the engine's answer to the foremen question: the client
// that gave it, for the switches --foremen makes, and, per pair, the
// projects whose foreman is on it, in the engine's order.
type engineForemen struct {
	client *api.Client
	byPair map[string][]agentsForeman
}

// askForemen asks the engine — started on demand, as brigada project list
// starts it — which projects have their foremen on one of pairs, harness
// and model both, since one model name can sit on more than one harness.
// An engine that cannot be asked answers ok=false, its reason on stderr;
// the command then points at brigada project list rather than name anyone.
func askForemen(ctx context.Context, rt *runtime, stderr io.Writer, pairs ...string) (*engineForemen, bool) {
	client, code := connectAgents(ctx, rt, "client", false, stderr)
	if code != 0 {
		return nil, false
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		fail(stderr, err)
		return nil, false
	}
	answer := engineForemen{client: client, byPair: map[string][]agentsForeman{}}
	for _, pair := range pairs {
		answer.byPair[pair] = nil
	}
	for _, p := range projects {
		pair := p.Harness + "/" + p.Model
		if _, asked := answer.byPair[pair]; !asked {
			continue
		}
		f := agentsForeman{repo: p.RepoURL}
		// The list carries the pair alone; the effort is read back per
		// project, and a read that fails leaves it unknown.
		if setting, err := client.ProjectForeman(ctx, p.RepoURL); err == nil {
			f.effort = setting.Effort
		}
		answer.byPair[pair] = append(answer.byPair[pair], f)
	}
	return &answer, true
}

// keptEffort is the effort a switch of a foreman to the pair passes on:
// the saved one when the target harness accepts it — the engine validates
// an effort per harness — and "" otherwise, which leaves the effort to the
// CLI's own default.
func keptEffort(pair, effort string) string {
	harness, _, _ := strings.Cut(pair, "/")
	if adapter.ValidateEffort(harness, effort) == nil {
		return effort
	}
	return ""
}

// foremanSwitch is the brigada project foreman command that switches repo's
// foreman to the pair. It carries --effort exactly when the foreman has an
// effort the target accepts: a switch typed without it clears the effort.
func foremanSwitch(repo, pair, effort string) string {
	harness, model, _ := strings.Cut(pair, "/")
	command := fmt.Sprintf("brigada project foreman --repo %s --harness %s --model %s", repo, harness, model)
	if kept := keptEffort(pair, effort); kept != "" {
		command += " --effort " + kept
	}
	return command
}

// effortOutcome says what a switch did, or would do, with the foreman's
// saved effort: nothing when none is set, it keeps one the pair accepts,
// and one it refuses is left to the CLI's own default.
func effortOutcome(pair, effort string) string {
	switch {
	case effort == "":
		return ""
	case keptEffort(pair, effort) != "":
		return ", keeping effort=" + effort
	}
	return "; effort=" + effort + " is not accepted for " + pair + ", so the CLI's own default applies"
}

// foremanTarget is the registered pair a foreman on the harness is switched
// to, from the roster a removal leaves: one on that harness first — a model
// change there keeps the project's session — else the first on another
// harness a foreman can run on. "" when the roster holds none.
func foremanTarget(left []agentSpec, harness string) string {
	var other string
	for _, spec := range left {
		if !adapter.ResumeCapable(spec.harness) {
			continue
		}
		if spec.harness == harness {
			return agentPair(spec)
		}
		if other == "" {
			other = agentPair(spec)
		}
	}
	return other
}

// printRemovedForemen names the projects whose foreman is the pair just
// removed, as the engine lists them: that foreman's turns wait until
// another registered agent offers the pair or the foreman is switched to a
// pair an agent offers, so each name comes with the switch to one. An
// engine that cannot be asked leaves it pointing at brigada project list.
func printRemovedForemen(ctx context.Context, rt *runtime, stdout, stderr io.Writer, pair, harness string, left []agentSpec) {
	answer, ok := askForemen(ctx, rt, stderr, pair)
	if !ok {
		fmt.Fprintln(stdout, "If this agent was a project's foreman, its turns wait until another registered agent offers the pair (brigada project list shows them).")
		return
	}
	named := answer.byPair[pair]
	if len(named) == 0 {
		fmt.Fprintf(stdout, "No project has %s as its foreman.\n", pair)
		return
	}
	if len(named) == 1 {
		fmt.Fprintf(stdout, "1 project has %s as its foreman; its turns wait until another registered agent offers the pair, however long that takes, or the foreman is switched to a pair an agent offers:\n", pair)
	} else {
		fmt.Fprintf(stdout, "%d projects have %s as their foremen; their turns wait until another registered agent offers the pair, however long that takes, or each foreman is switched to a pair an agent offers:\n", len(named), pair)
	}
	target := foremanTarget(left, harness)
	for _, f := range named {
		fmt.Fprintf(stdout, "  %s\n", f.repo)
		if target == "" {
			fmt.Fprintln(stdout, "    no registered agent offers a pair a foreman can run on; brigada agents add <harness>/<model> registers one")
			continue
		}
		fmt.Fprintf(stdout, "    %s\n", foremanSwitch(f.repo, target, f.effort))
	}
}

// notRegisteredHint is the second half of a not-registered refusal. Top and
// the job cells show an agent as a short pair — the harness and the model's
// last segment (topShortAgent) — which is not a pair the agents file
// registers; when the refused pair is exactly one saved pair's short form,
// the hint names that pair, else it points at the listing that shows pairs
// whole.
func notRegisteredHint(pair string, saved []string) string {
	var full []string
	for _, have := range saved {
		if topShortAgent(have) == topShortAgent(pair) {
			full = append(full, have)
		}
	}
	if len(full) == 1 {
		return fmt.Sprintf("the saved pair is %s", full[0])
	}
	return "brigada capacity lists the registered agents"
}

// restartRunnerForAgents is the shared tail of agents add, agents remove and
// agents update: a running runner is handed a restart request it honours
// between jobs, so no running job is cut — unless now asked for exactly
// that. Without one there is nothing to restart; the changed lines serve
// from the next start.
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
		fmt.Fprintf(stdout, "runner already running (pid %d); the change serves after its next restart\n", result.Record.PID)
	} else {
		fmt.Fprintf(stdout, "runner restarted (pid %d)\n", result.Record.PID)
	}
	return 0
}

// agentsUpdateCandidate is one superseded pair's rewrite: the raw line it
// replaces, the line replacing it, the saved effort the rewrite had to
// drop when the new model refused it, and — once the engine answered —
// the projects whose foreman is on the old pair, with whether another line
// of the file keeps registering the pair once the rewrite lands.
type agentsUpdateCandidate struct {
	oldPair, newPair string
	oldLine, newLine string
	effortDropped    string
	foremen          []agentsForeman
	served           bool
}

// runAgentsUpdate is `brigada agents update`: every registered agent whose
// model a newer version of the same line replaces is rewritten to the newer
// model in place, each rewrite accepted interactively or by --yes; each
// rewrite's lines name the projects whose foreman is on the moved pair,
// which --foremen also switches, and a running runner restarts to serve
// the new lines.
func runAgentsUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents update",
		"brigada agents update [--yes] [--dry-run] [--foremen] [--now]",
		"Move registered agents to the newer model of their line the CLIs list: rewrite each superseded line of the agents file in place, keeping every other line, name the projects whose foreman is on a moved pair, and restart the runner to serve the new models.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	yes := fs.Bool("yes", false, "apply every update without prompting")
	dryRun := fs.Bool("dry-run", false, "print the rewrites and exit: nothing is written and the runner is not restarted")
	foremen := fs.Bool("foremen", false, "with every applied rewrite, switch the projects whose foreman is on the moved pair to the newer model, keeping an effort it accepts; --dry-run lists the switches")
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
		// is left alone. onPair counts the lines that register the old
		// pair: the rewrite replaces the first, and any other keeps
		// offering the pair once the rewrite lands.
		var oldLine string
		var spec agentSpec
		onPair := 0
		for _, line := range lines {
			var one agentSpecs
			if one.Set(line) != nil || agentPair(one.specs[0]) != oldPair {
				continue
			}
			onPair++
			if oldLine == "" {
				oldLine, spec = line, one.specs[0]
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
		c := agentsUpdateCandidate{oldPair: oldPair, newPair: newPair, oldLine: oldLine, newLine: spec.String(), served: onPair > 1}
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
	// Foremen are the engine's knowledge, so the command asks it once for
	// every moved pair before any rewrite is printed, and each rewrite's
	// lines name what it answered. The engine never picks a pair on its
	// own: it lists, and the switch is the rewrite's newer model.
	moved := make([]string, len(candidates))
	for i, c := range candidates {
		moved[i] = c.oldPair
	}
	var named *engineForemen
	asked := false
	if len(moved) > 0 {
		if named, asked = askForemen(ctx, rt, stderr, moved...); asked {
			for i := range candidates {
				candidates[i].foremen = named.byPair[candidates[i].oldPair]
			}
		}
	}
	printCandidate := func(c agentsUpdateCandidate) {
		fmt.Fprintf(stdout, "%s → %s\n  %s\n", c.oldPair, c.newPair, c.newLine)
		if c.effortDropped != "" {
			fmt.Fprintf(stdout, "  note: effort=%s is not accepted for %s; the line leaves the effort to the CLI's default\n", c.effortDropped, c.newPair)
		}
		if !asked {
			fmt.Fprintf(stdout, "  projects whose foreman is on %s: brigada project list shows them (the engine could not be asked)\n", c.oldPair)
			return
		}
		n := len(c.foremen)
		if n == 0 {
			return
		}
		// The switch is typed out per project with --effort exactly where
		// --foremen leaves the effort, so both end the same way.
		whose, switched, head := "their", "each foreman is switched", fmt.Sprintf("  %d projects have %s as their foremen", n, c.oldPair)
		if n == 1 {
			whose, switched, head = "its", "the foreman is switched", fmt.Sprintf("  1 project has %s as its foreman", c.oldPair)
		}
		if c.served {
			head += "; another line still offers the pair, so " + whose + " turns do not wait"
		} else {
			head += "; no other line offers the pair once the rewrite lands, so " + whose + " turns wait until " + switched
		}
		fmt.Fprintln(stdout, head+":")
		for _, f := range c.foremen {
			fmt.Fprintf(stdout, "    %s\n      %s\n", f.repo, foremanSwitch(f.repo, c.newPair, f.effort))
			if *dryRun && *foremen {
				fmt.Fprintf(stdout, "      with --foremen: switches the foreman to %s%s\n", c.newPair, effortOutcome(c.newPair, f.effort))
			}
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
	// A switch that fails is reported and leaves the rewrite standing, so
	// the rewrites all land and the command fails at the end instead.
	failed := 0
	for _, c := range accepted {
		if err := runnerstate.ReplaceAgent(dir, c.oldLine, c.newLine); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "updated %s → %s", c.oldPair, c.newPair)
		if c.effortDropped != "" {
			fmt.Fprintf(stdout, " (effort=%s refused; the CLI's default applies)", c.effortDropped)
		}
		fmt.Fprintln(stdout)
		if *foremen && asked {
			failed += switchForemen(ctx, named.client, c, stdout, stderr)
		}
	}
	if *foremen && !asked {
		fmt.Fprintln(stderr, "brigada: agents update: no foreman was switched: the engine could not be asked which projects have their foremen on a moved pair; brigada project list shows them, and brigada project foreman switches one")
		failed++
	}
	fmt.Fprintf(stdout, "rewritten in %s\n", initHomePath(filepath.Join(dir, "agents")))
	if code := restartRunnerForAgents(ctx, rt, *now, stdout, stderr); code != 0 {
		return code
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// switchForemen switches the foremen an applied rewrite named to the newer
// model, as brigada project foreman does, and says so per project: each
// keeps an effort the newer model accepts and otherwise leaves the effort
// to the CLI's own default. A switch that fails is reported, with the
// command that makes it, and the rewrite stands; the return is how many
// failed.
func switchForemen(ctx context.Context, client *api.Client, c agentsUpdateCandidate, stdout, stderr io.Writer) int {
	harness, model, _ := strings.Cut(c.newPair, "/")
	failed := 0
	for _, f := range c.foremen {
		if _, err := client.SetProjectForeman(ctx, f.repo, harness, model, keptEffort(c.newPair, f.effort)); err != nil {
			fmt.Fprintf(stderr, "brigada: agents update: the foreman of %s was not switched to %s (%v); the rewrite stands, and %s switches it\n", f.repo, c.newPair, err, foremanSwitch(f.repo, c.newPair, f.effort))
			failed++
			continue
		}
		fmt.Fprintf(stdout, "foreman of %s switched to %s%s\n", f.repo, c.newPair, effortOutcome(c.newPair, f.effort))
	}
	return failed
}
