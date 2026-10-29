package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/adapter"
	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// runProject is the project command's dispatch: `project foreman`, the
// foreman's harness and model (§4.1) — the user's choice of which CLI
// runs the foreman and which model it runs on — and `project list`, every
// project the engine knows.
func runProject(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatch(ctx, args, stdout, stderr, "project", "usage: brigada project foreman|list|set|show|import [--repo URL] ...", "foreman, list, set, show or import", projectCommands)
}

// runProjectList prints one line per project: the repo URL, the foreman's
// harness/model ("-" while none is chosen), the active plans and what
// waits on the user.
func runProjectList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project list",
		"brigada project list [--no-start]",
		"List projects with their foreman, active plans and needs-you count.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	noStart := fs.Bool("no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "project list takes no positional arguments, got %q", extra)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", *noStart, stderr)
	if code != 0 {
		return code
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if len(projects) == 0 {
		fmt.Fprintln(stdout, "no projects; create one: brigada project foreman --repo <url> --harness <harness> --model <model>")
		return 0
	}
	for _, p := range projects {
		foreman := "-"
		if p.Harness != "" && p.Model != "" {
			foreman = p.Harness + "/" + p.Model
		}
		fmt.Fprintf(stdout, "%s  foreman=%s  active_plans=%d  needs_you=%d\n", p.RepoURL, foreman, p.ActivePlans, p.NeedsYou)
	}
	return 0
}

// runProjectSettings is `project set` and `project show` (§6.3): every
// project setting in engine.SettingKeys. set takes key=value pairs after the
// flags and prints the settings as they now stand; show prints every key
// with its effective value and whether it is the default. --repo
// defaults to the working directory's origin. Values are validated by
// the engine's own parser before the engine is contacted, so a value
// accepted here is one the limit understands.
func runProjectSettings(ctx context.Context, verb string, args []string, stdout, stderr io.Writer) int {
	synopsis := "brigada project " + verb + " [--repo URL] [--no-start]"
	description := "Show a project’s settings and where each value comes from."
	if verb == "set" {
		synopsis += " key=value ..."
		description = "Set project settings, including approval limits and worker instructions."
	}
	fs := newFlagSet("project "+verb, synopsis, description)
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		repo    string
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the repository URL (default: the working directory's origin)")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	validKeys := strings.Join(engine.SettingKeys, ", ")
	values := map[string]string{}
	roleValues := map[string]string{}
	pairs := fs.Args()
	if verb == "show" && len(pairs) > 0 {
		return usagef(stderr, "project show takes no positional arguments, got %q", pairs)
	}
	if verb == "set" && len(pairs) == 0 {
		return usagef(stderr, "usage: brigada project set [--repo URL] key=value ...; valid keys: %s", validKeys)
	}
	for _, p := range pairs {
		key, value, ok := strings.Cut(p, "=")
		if !ok {
			return usagef(stderr, "project set wants key=value, got %q; valid keys: %s", p, validKeys)
		}
		if _, dup := values[key]; dup {
			return usagef(stderr, "project set names %s twice", key)
		}
		if role, ok := strings.CutPrefix(key, "instructions."); ok {
			// instructions.<role>=@file|@-|text sets one role's local
			// override (§5.9); the plain `instructions` key stays the
			// project-wide worker text it always was.
			if err := engine.ValidateRoleText(role, ""); err != nil {
				return usagef(stderr, "%v", err)
			}
			if _, dup := roleValues[role]; dup {
				if _, alias := values[engine.SettingInstructions]; alias && role == engine.RoleWorker {
					return usagef(stderr, "project set names instructions and instructions.worker, which are the same text; give one")
				}
				return usagef(stderr, "project set names %s twice", key)
			}
			if strings.HasPrefix(value, "@") {
				text, err := readInstructionsArg(value[1:])
				if err != nil {
					return fail(stderr, err)
				}
				value = text
			}
			if err := engine.ValidateRoleText(role, value); err != nil {
				return usagef(stderr, "%v", err)
			}
			roleValues[role] = value
			continue
		}
		if key == engine.SettingInstructions && strings.HasPrefix(value, "@") {
			// instructions=@file (or @- for stdin) reads the text here: the
			// engine takes the text, never a path (§5.9).
			text, err := readInstructionsArg(value[1:])
			if err != nil {
				return fail(stderr, err)
			}
			value = text
		}
		if err := engine.ValidateSettingValue(key, value); err != nil {
			return usagef(stderr, "%v", err)
		}
		values[key] = value
		if key == engine.SettingInstructions && value != "" {
			// The old name is the worker override (§5.9): the text also
			// sets the worker role, and no other. The project-wide setting
			// keeps it too, for the readers that still use it.
			if _, dup := roleValues[engine.RoleWorker]; dup {
				return usagef(stderr, "project set names instructions and instructions.worker, which are the same text; give one")
			}
			roleValues[engine.RoleWorker] = value
		}
	}
	if repo == "" {
		dir, err := os.Getwd()
		if err != nil {
			return fail(stderr, fmt.Errorf("find the working directory: %w", err))
		}
		if repo, err = gitOriginURL(dir); err != nil {
			return fail(stderr, err)
		}
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	var (
		got engine.ProjectSettings
		err error
	)
	if verb == "set" {
		if len(roleValues) > 0 {
			if err := client.SetRoleInstructions(ctx, repo, roleValues); err != nil {
				return fail(stderr, err)
			}
		}
		if len(values) > 0 {
			got, err = client.SetProjectSettings(ctx, repo, values)
		} else {
			got, err = client.ProjectSettings(ctx, repo)
		}
	} else {
		got, err = client.ProjectSettings(ctx, repo)
		var se *api.StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return fail(stderr, fmt.Errorf("no project for repo %s; set one of its settings or choose its foreman first", repo))
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	printSettings(stdout, got)
	if verb == "show" {
		printGuidance(ctx, client, stdout, got.ProjectID)
	}
	return 0
}

// printSettings prints every key with its value and provenance (§13.1):
// (default), (set) for a local override, or (imported <hash>) for a value
// the last project.yaml import set.
func printSettings(stdout io.Writer, got engine.ProjectSettings) {
	for _, s := range got.Settings {
		state := "set"
		switch {
		case s.Default:
			state = "default"
		case s.Source == engine.SourceImported:
			state = "imported " + shortHash(s.Hash)
		}
		fmt.Fprintf(stdout, "%s=%s (%s)\n", s.Key, s.Value, state)
	}
}

// readInstructionsArg reads the text of `project set instructions=@path`
// (@- is stdin), bounded like the engine bounds it: a text over the limit
// is refused, never cut (§5.9).
func readInstructionsArg(path string) (string, error) {
	var r io.Reader = projectStdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("read the instructions: %w", err)
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, engine.MaxInstructionsBytes+1))
	if err != nil {
		return "", fmt.Errorf("read the instructions: %w", err)
	}
	return string(b), nil
}

// readInstructionsFile reads the file project.yaml's instructions_file
// names (§5.9), for the import: the path must stay inside the repository
// at root after symlinks are resolved and name a regular file, and the
// text is bounded and checked as the engine will check it. Refusals name
// the path. An empty file is returned as "": an unset value.
func readInstructionsFile(root, rel string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("instructions_file %s: resolve the repository root: %w", rel, err)
	}
	full, err := filepath.EvalSymlinks(filepath.Join(realRoot, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("instructions_file %s: %w", rel, err)
	}
	if inside, err := filepath.Rel(realRoot, full); err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("instructions_file %s resolves outside the repository", rel)
	}
	st, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("instructions_file %s: %w", rel, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("instructions_file %s is not a regular file", rel)
	}
	if st.Size() > engine.MaxInstructionsBytes {
		return "", fmt.Errorf("instructions_file %s is %d bytes, over the %d byte limit; shorten it (nothing is truncated)", rel, st.Size(), engine.MaxInstructionsBytes)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("instructions_file %s: %w", rel, err)
	}
	if err := engine.ValidateInstructions(string(b)); err != nil {
		return "", fmt.Errorf("instructions_file %s: %s", rel, strings.TrimPrefix(err.Error(), engine.ErrInvalidSetting.Error()+": "))
	}
	return string(b), nil
}

// readRoleFile reads the file the nested instructions map names for a
// role (§5.9), by the rules of readInstructionsFile: inside the
// repository after symlinks, a regular file, bounded at
// engine.MaxRoleTextBytes before the engine checks it again. Every
// refusal names the key and the path. An empty file returns "": empty by
// choice.
func readRoleFile(root, role, rel string) (string, error) {
	label := "instructions." + role + " " + rel
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("%s: resolve the repository root: %w", label, err)
	}
	full, err := filepath.EvalSymlinks(filepath.Join(realRoot, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	if inside, err := filepath.Rel(realRoot, full); err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s resolves outside the repository", label)
	}
	st, err := os.Stat(full)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", label)
	}
	if st.Size() > engine.MaxRoleInstructionBytes+maxHeaderBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d byte limit; shorten it (nothing is truncated)", label, st.Size(), engine.MaxRoleInstructionBytes)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	// The version header is no part of the text: the stored text and its
	// digest are those of the file without it (§5.9).
	text := stripRoleHeader(string(b))
	if err := engine.ValidateRoleText(role, text); err != nil {
		return "", fmt.Errorf("%s: %s", label, strings.TrimPrefix(err.Error(), engine.ErrInvalidSetting.Error()+": "))
	}
	return text, nil
}

// readRoleFiles reads every role file cfg names; a role it leaves out is
// absent from the result: not configured.
func readRoleFiles(root string, cfg engine.ProjectConfig) (map[string]string, error) {
	var roles map[string]string
	for _, role := range engine.RoleKeys {
		rel, ok := cfg.RoleFiles[role]
		if !ok {
			continue
		}
		text, err := readRoleFile(root, role, rel)
		if err != nil {
			return nil, err
		}
		if roles == nil {
			roles = map[string]string{}
		}
		roles[role] = text
	}
	return roles, nil
}

// shortHash is the first 12 characters of a content hash.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// projectStdin is where the import's confirmation is read from; a test
// replaces it.
var projectStdin io.Reader = os.Stdin

// gitRoot returns the top of the repository containing dir.
func gitRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("the working directory %s is not inside a git repository", dir)
	}
	return strings.TrimSpace(string(out)), nil
}

// runProjectImport is `project import` (§13.1): the client reads the
// repo's .brigada/project.yaml and sends its content; the engine never
// reads a repo path. A first import over hand-set values prints the diff
// and asks (--yes accepts); later imports apply a file change over a
// still-imported value and print a conflict, with both values, for a
// local override.
func runProjectImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runProjectImportWithConfirm(ctx, args, stdout, stderr, func(prompt string) (bool, error) {
		fmt.Fprint(stdout, prompt+" [y/N] ")
		answer, _ := bufio.NewReader(projectStdin).ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(answer))
		confirmed := a == "y" || a == "yes"
		if !confirmed {
			fmt.Fprintln(stdout)
		}
		return confirmed, nil
	})
}

// Init supplies its prompt handling so an import confirmation follows the same
// terminal formatting and EOF rules as the rest of setup.
func runProjectImportWithConfirm(ctx context.Context, args []string, stdout, stderr io.Writer, confirm func(string) (bool, error)) int {
	fs := newFlagSet("project import",
		"brigada project import [--repo URL] [--file PATH] [--yes] [--no-start]",
		"Import the repository’s .brigada/project.yaml settings.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		repo    string
		file    string
		yes     bool
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the repository URL the file must name (default: the working directory's origin)")
	fs.StringVar(&file, "file", "", "the project.yaml to read (default: "+engine.ProjectConfigPath+" in the working directory's repository)")
	fs.BoolVar(&yes, "yes", false, "accept replacing hand-set values on a first import")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "project import takes no positional arguments, got %q", extra)
	}
	dir, err := os.Getwd()
	if err != nil {
		return fail(stderr, fmt.Errorf("find the working directory: %w", err))
	}
	if repo == "" {
		if repo, err = gitOriginURL(dir); err != nil {
			return fail(stderr, err)
		}
	}
	if file == "" {
		root, err := gitRoot(dir)
		if err != nil {
			return fail(stderr, err)
		}
		file = filepath.Join(root, filepath.FromSlash(engine.ProjectConfigPath))
	}
	content, err := os.ReadFile(file)
	if err != nil {
		return fail(stderr, fmt.Errorf("read the project configuration: %w", err))
	}
	// Refuse a bad file before contacting the engine; the engine validates
	// again, since it is the one that stores.
	cfg, err := engine.ParseProjectConfig(content)
	if err != nil {
		return fail(stderr, err)
	}
	// A file that names an instructions_file has the client read it: the
	// engine takes the text, never the path (§5.9).
	var instructions *string
	var roles map[string]string
	if len(cfg.RoleFiles) > 0 {
		root, err := gitRoot(filepath.Dir(file))
		if err != nil {
			return fail(stderr, err)
		}
		if roles, err = readRoleFiles(root, cfg); err != nil {
			return fail(stderr, err)
		}
		for _, role := range engine.RoleKeys {
			text, ok := roles[role]
			switch {
			case !ok || strings.TrimSpace(text) != "":
			case role == engine.RoleWorker:
				fmt.Fprintf(stdout, "instructions.%s %s is empty: the project's instructions are unset and the default worker text applies\n", role, cfg.RoleFiles[role])
			default:
				fmt.Fprintf(stdout, "instructions.%s %s is empty: the role runs on the contract alone\n", role, cfg.RoleFiles[role])
			}
		}
		// The worker's file is instructions_file spelled in the map
		// (§5.9): its text is the instructions setting and rides the
		// import beside the file, in its hash.
		if text, ok := roles[engine.RoleWorker]; ok {
			instructions = &text
			delete(roles, engine.RoleWorker)
		}
	}
	// A skills_dir has the client parse every skill under it; the engine
	// takes their texts and digests, never the directory (§5.10).
	var skills *[]engine.Skill
	if cfg.SkillsDir != "" {
		root, err := gitRoot(filepath.Dir(file))
		if err != nil {
			return fail(stderr, err)
		}
		parsed, err := readSkills(root, cfg.SkillsDir)
		if err != nil {
			return fail(stderr, err)
		}
		if parsed == nil {
			parsed = []engine.Skill{}
		}
		skills = &parsed
	}
	if cfg.InstructionsFile != "" {
		root, err := gitRoot(filepath.Dir(file))
		if err != nil {
			return fail(stderr, err)
		}
		text, err := readInstructionsFile(root, cfg.InstructionsFile)
		if err != nil {
			return fail(stderr, err)
		}
		instructions = &text
		if strings.TrimSpace(text) == "" {
			fmt.Fprintf(stdout, "instructions_file %s is empty: the project's instructions are unset\n", cfg.InstructionsFile)
		}
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	res, err := client.ImportProjectConfigFull(ctx, repo, content, instructions, roles, skills, yes)
	if err != nil {
		return fail(stderr, err)
	}
	if res.NeedsConfirm {
		fmt.Fprintln(stdout, "first import: these hand-set values would be replaced by the file's:")
		for _, d := range res.Diffs {
			fmt.Fprintf(stdout, "  %s: hand-set %s -> file %s\n", d.Key, d.Old, d.New)
		}
		confirmed, err := confirm("replace them?")
		if err != nil {
			return fail(stderr, err)
		}
		if !confirmed {
			fmt.Fprintln(stdout, "not imported; nothing changed (--yes accepts the replacement)")
			return 1
		}
		if res, err = client.ImportProjectConfigFull(ctx, repo, content, instructions, roles, skills, true); err != nil {
			return fail(stderr, err)
		}
	}
	fmt.Fprintf(stdout, "imported %s (hash %s)\n", engine.ProjectConfigPath, shortHash(res.Hash))
	for _, c := range res.Applied {
		fmt.Fprintf(stdout, "  %s: %s -> %s\n", c.Key, c.Old, c.New)
	}
	for _, c := range res.Conflicts {
		if strings.HasPrefix(c.Key, "instructions.") {
			// A role text conflict shows both texts; the local one stands
			// until the user sets the text they want (§5.9).
			fmt.Fprintf(stdout, "conflict: %s: local override %s kept, file has %s; to take the file's text: brigada project set %s=@<file>\n",
				c.Key, c.Old, c.New, c.Key)
			continue
		}
		fmt.Fprintf(stdout, "conflict: %s: local override %s kept, file has %s; to take the file's value: brigada project set %s= then import again\n",
			c.Key, c.Old, c.New, c.Key)
	}
	if res.Settings != nil {
		printSettings(stdout, *res.Settings)
	}
	return 0
}

// reportGuidanceChanges flags the role files and skills whose current
// digests differ from the imported ones (§5.9, §5.10): the client reports
// the digests of what is on disk, the engine compares, and nothing stored
// changes until the next import. Silent when nothing is configured or
// nothing changed.
func reportGuidanceChanges(ctx context.Context, client *api.Client, root string, content []byte, project int64, stdout io.Writer) {
	cfg, err := engine.ParseProjectConfig(content)
	if err != nil || project == 0 || (len(cfg.RoleFiles) == 0 && cfg.SkillsDir == "") {
		return
	}
	st, err := client.StatusWithFileDigests(ctx, project, currentFileDigests(root, cfg))
	if err != nil {
		return
	}
	// The file each role reads is the client's knowledge (project.yaml),
	// so status names it beside the engine's per-role line (§5.9).
	for _, role := range engine.RoleKeys {
		if p, ok := cfg.RoleFiles[role]; ok {
			fmt.Fprintf(stdout, "instructions.%s: file %s\n", role, p)
		}
	}
	for _, line := range guidanceChangeLines(st, project, cfg, root) {
		fmt.Fprintln(stdout, line)
	}
}

// reportProjectConfigStatus is status's flag (§13.1) for a project whose
// committed file differs from what was last imported. The client knows the
// file because it runs in the repo; the engine never looks. It only
// reports: nothing is imported by it. Silent when the working directory is
// no repo with the file, or the engine has no project for its origin.
func reportProjectConfigStatus(ctx context.Context, client *api.Client, stdout io.Writer) {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	root, err := gitRoot(dir)
	if err != nil {
		return
	}
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(engine.ProjectConfigPath)))
	if err != nil {
		return
	}
	origin, err := gitOriginURL(dir)
	if err != nil {
		return
	}
	got, err := client.ProjectSettings(ctx, origin)
	if err != nil {
		return
	}
	var instructions *string
	if cfg, err := engine.ParseProjectConfig(content); err == nil {
		if rel, ok := cfg.RoleFiles[engine.RoleWorker]; ok {
			if text, err := readRoleFile(root, engine.RoleWorker, rel); err == nil {
				instructions = &text
			}
		} else if cfg.InstructionsFile != "" {
			if text, err := readInstructionsFile(root, cfg.InstructionsFile); err == nil {
				instructions = &text
			}
		}
	}
	hash := engine.ConfigHashWith(content, instructions)
	defer reportGuidanceChanges(ctx, client, root, content, got.ProjectID, stdout)
	switch {
	case got.ImportHash == "":
		fmt.Fprintf(stdout, "%s: not imported yet (brigada project import)\n", engine.ProjectConfigPath)
	case got.ImportHash != hash:
		fmt.Fprintf(stdout, "%s: changed since the last import (file %s, imported %s); brigada project import applies it\n",
			engine.ProjectConfigPath, shortHash(hash), shortHash(got.ImportHash))
	}
}

// knownProjectsHint names the projects the engine has, for the error of a
// command that could not map its repo to one: the URLs to pass to --repo.
// Empty when the engine cannot be asked, so the original error stands
// alone.
func knownProjectsHint(ctx context.Context, client *api.Client) string {
	projects, err := client.Projects(ctx)
	if err != nil {
		return ""
	}
	if len(projects) == 0 {
		return "the engine has no projects yet"
	}
	urls := make([]string, len(projects))
	for i, p := range projects {
		urls[i] = p.RepoURL
	}
	return "known projects: " + strings.Join(urls, ", ")
}

// isNotFound reports whether err is the API's 404.
func isNotFound(err error) bool {
	var se *api.StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// runProjectForeman shows or sets the foreman's harness and model for a
// repo (§4.1): with --harness and --model it chooses them — the harness
// must resume headless, which the engine refuses otherwise — and prints
// the stored choice; with neither it just prints what is stored, empty
// fields and all.
func runProjectForeman(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project foreman",
		"brigada project foreman [--repo URL] [--harness HARNESS --model MODEL [--effort LEVEL]] [--no-start]",
		"Show or choose a project’s foreman harness, model and reasoning effort.")
	stderr = commandStderr(stderr, fs.Name())
	common := addCommon(fs)
	var (
		repo    string
		harness string
		model   string
		effort  string
		noStart bool
	)
	fs.StringVar(&repo, "repo", "", "the repository URL; empty = the origin of the working directory")
	fs.StringVar(&harness, "harness", "", "the foreman's harness (claude or codex: a CLI that resumes headless); empty = show the choice")
	fs.StringVar(&model, "model", "", "the foreman's model; goes with --harness")
	fs.StringVar(&effort, "effort", "", "the foreman's reasoning effort, validated per harness (claude: low|medium|high|xhigh|max; codex: none|minimal|low|medium|high|xhigh|max|ultra); goes with --harness; empty = the CLI's default")
	fs.BoolVar(&noStart, "no-start", false, "do not start the engine on demand; fail when none is running")
	if code, stop := common.parse(stdout, stderr, args); stop {
		return code
	}
	if extra := fs.Args(); len(extra) > 0 {
		return usagef(stderr, "project foreman takes no positional arguments, got %q", extra)
	}
	if repo == "" {
		// Like attach: inside a repo, the project is the one its origin names.
		dir, err := os.Getwd()
		if err != nil {
			return fail(stderr, fmt.Errorf("find the working directory: %w", err))
		}
		origin, err := gitOriginURL(dir)
		if err != nil {
			return usagef(stderr, "project foreman needs --repo URL outside a repository (%v)", err)
		}
		repo = origin
	}
	if (harness == "") != (model == "") {
		return usagef(stderr, "project foreman sets --harness and --model together; with neither it shows the choice")
	}
	if harness == "" && effort != "" {
		return usagef(stderr, "project foreman sets --effort with --harness and --model; with none of them it shows the choice")
	}
	if err := adapter.ValidateEffort(harness, effort); err != nil {
		return usagef(stderr, "--effort: %v", err)
	}
	rt, code := resolveRuntime(common, stderr)
	if code != 0 {
		return code
	}
	client, code := connect(ctx, rt, "client", noStart, stderr)
	if code != 0 {
		return code
	}
	if harness == "" {
		setting, err := client.ProjectForeman(ctx, repo)
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "harness=%s model=%s%s\n", setting.Harness, setting.Model, effortField(setting.Effort))
		return 0
	}
	setting, err := client.SetProjectForeman(ctx, repo, harness, model, effort)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "foreman set: harness=%s model=%s%s\n", setting.Harness, setting.Model, effortField(setting.Effort))
	if setting.Unserved {
		// The choice stands, but a foreman turn runs only on an agent that
		// offers exactly this pair: until one is registered the turns wait.
		fmt.Fprintf(stderr, "warning: no registered runner agent offers %[1]s/%[2]s; foreman turns will wait until one does. Start a runner (another one, if yours already offers %[1]s) with --agent harness=%[1]s,model=%[2]s\n",
			setting.Harness, setting.Model)
	}
	return 0
}

// agentName is an agent's display name, harness/model, with @effort only
// where an effort is set: claude/opus@high.
func agentName(harness, model, effort string) string {
	name := harness + "/" + model
	if effort != "" {
		name += "@" + effort
	}
	return name
}

// effortField is the " effort=<level>" suffix of a printed foreman choice;
// empty when no effort is set, so an unset effort prints as before.
func effortField(effort string) string {
	if effort == "" {
		return ""
	}
	return " effort=" + effort
}
