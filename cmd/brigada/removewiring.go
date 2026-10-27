package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Blackfire-AI/brigada/internal/engine"
)

// legacyWiringSteps lists the wiring-removal steps brigada remove and a
// rerun of init share: strip everything an older init wired into the
// harness, and nothing of the user's.
func legacyWiringSteps(root string) []wireStep {
	owned := func(what string) func(d []byte, exists bool) ([]byte, string, error) {
		return func(d []byte, exists bool) ([]byte, string, error) {
			if !exists {
				return d, "unchanged", nil
			}
			if !bytes.Contains(d, []byte(wiringSkillMarker)) {
				return nil, "", errors.New("no brigada marker; keeping your " + what)
			}
			return nil, "removed", nil
		}
	}
	steps := []wireStep{
		{wiringMCPFile, removeMCPServer},
		{wiringCodexFile, removeCodexServer},
		{wiringSettingsFile, func(d []byte, exists bool) ([]byte, string, error) {
			return removeWiringHooks(d, exists, false)
		}},
		{wiringSharedSettingsFile, func(d []byte, exists bool) ([]byte, string, error) {
			return removeWiringHooks(d, exists, true)
		}},
		{wiringRulesFile, owned("file")},
		{wiringSkillFile, owned("skill")},
	}
	// A shared file that is a link to the other one holds no section of its
	// own: the other is the file. Any other symlink is refused by planSteps.
	for _, pair := range [][2]string{{wiringAgentsFile, wiringClaudeFile}, {wiringClaudeFile, wiringAgentsFile}} {
		if rel := pair[0]; !linksTo(root, rel, pair[1]) {
			steps = append(steps, wireStep{rel, func(d []byte, exists bool) ([]byte, string, error) {
				return removeSections(rel, d, exists)
			}})
		}
	}
	return steps
}

// planExcludeRemoval plans the edit of git's exclude file that drops the
// line ignoring brigada's private settings; a linked worktree's shared
// exclude file is located through git.
func planExcludeRemoval(root string) wireFile {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return wireFile{rel: "git info/exclude", err: err}
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	exclude := planSteps(filepath.Dir(path), []wireStep{{filepath.Base(path), removeSettingsExclude}})[0]
	exclude.rel = filepath.ToSlash(strings.TrimSpace(string(out)))
	return exclude
}

// planRemoval reads the same paths as init, including a linked worktree's
// shared exclude file. nil next means delete, only for this removal plan.
func planRemoval(root string, deleteConfig bool) []wireFile {
	steps := append(legacyWiringSteps(root), wireStep{operatorFile, removeOperatorFile}, wireStep{operatorSkillFile, removeOperatorSkill}, wireStep{engine.ProjectConfigPath, func(d []byte, exists bool) ([]byte, string, error) {
		if exists && deleteConfig {
			return nil, "removed", nil
		}
		return d, "unchanged", nil
	}})
	return append(planSteps(root, steps), planExcludeRemoval(root))
}

func removeSettingsExclude(d []byte, exists bool) ([]byte, string, error) {
	var out []byte
	for _, line := range bytes.SplitAfter(d, []byte("\n")) {
		if string(bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))) != "/"+wiringSettingsFile {
			out = append(out, line...)
		}
	}
	// Keep an existing exclude file, even if our line was its only content.
	if out == nil {
		out = []byte{}
	}
	return out, "removed", nil
}

func removeMCPServer(d []byte, exists bool) ([]byte, string, error) {
	if !exists {
		return d, "unchanged", nil
	}
	root, err := jsonRoot(d)
	if err != nil {
		return nil, "", err
	}
	mi := root.lookup("mcpServers")
	if mi < 0 {
		return d, "unchanged", nil
	}
	servers, err := jsonSub(d, root, mi, '{', "mcpServers")
	if err != nil {
		return nil, "", err
	}
	bi := servers.lookup("brigada")
	if bi < 0 {
		return d, "unchanged", nil
	}
	if len(servers.vals) == 1 {
		if len(root.vals) == 1 {
			return nil, "removed", nil
		}
		return jsonRemove(d, root, mi), "removed", nil
	}
	next := jsonRemove(d, servers, bi)
	// Remove duplicate keys too: decoders would otherwise expose an old entry.
	next, _, err = removeMCPServer(next, true)
	return next, "removed", err
}

// removeWiringHooks uses init's exact-command migration, restricted to Stop
// and yield for private settings. It also drops the emptied hooks wrapper.
func removeWiringHooks(d []byte, exists, shared bool) ([]byte, string, error) {
	if !exists {
		return d, "unchanged", nil
	}
	old := d
	for {
		next, removed, err := removeWiringHook(d, shared)
		if err != nil {
			return nil, "", err
		}
		if !removed {
			break
		}
		d = next
	}
	if bytes.Equal(old, d) {
		return old, "unchanged", nil
	}
	root, _ := jsonRoot(d)
	if hi := root.lookup("hooks"); hi >= 0 {
		hooks, err := jsonSub(d, root, hi, '{', "hooks")
		if err != nil {
			return nil, "", err
		}
		if len(hooks.vals) == 0 {
			d = jsonRemove(d, root, hi)
		}
	}
	root, _ = jsonRoot(d)
	if len(root.vals) == 0 {
		return nil, "removed", nil
	}
	return d, "removed", nil
}

func removeWiringHook(d []byte, shared bool) ([]byte, bool, error) {
	// The shared migration already removes exactly both legacy commands.
	if shared {
		return removeSharedHook(d)
	}
	root, err := jsonRoot(d)
	if err != nil {
		return nil, false, err
	}
	hi := root.lookup("hooks")
	if hi < 0 {
		return d, false, nil
	}
	hooks, err := jsonSub(d, root, hi, '{', "hooks")
	if err != nil {
		return nil, false, err
	}
	ei := hooks.lookup("Stop")
	if ei < 0 {
		return d, false, nil
	}
	list, err := jsonSub(d, hooks, ei, '[', "hooks.Stop")
	if err != nil {
		return nil, false, err
	}
	for gi, span := range list.vals {
		if d[span.start] != '{' {
			continue
		}
		group, _ := jsonParse(d, span.start)
		ii := group.lookup("hooks")
		if ii < 0 {
			continue
		}
		inner, err := jsonSub(d, group, ii, '[', "hooks.Stop.hooks")
		if err != nil {
			return nil, false, err
		}
		for i, hook := range inner.vals {
			if d[hook.start] != '{' {
				continue
			}
			h, _ := jsonParse(d, hook.start)
			ci := h.lookup("command")
			if ci < 0 || !jsonEqual(d[h.vals[ci].start:h.vals[ci].end], hookYieldCommand) {
				continue
			}
			if len(inner.vals) > 1 {
				return jsonRemove(d, inner, i), true, nil
			}
			if len(list.vals) > 1 {
				return jsonRemove(d, list, gi), true, nil
			}
			return jsonRemove(d, hooks, ei), true, nil
		}
	}
	return d, false, nil
}

// Check all files before archiving or editing any file, and check each again
// immediately before writing/deleting it, just as init does on writes.
func checkRemoval(files []wireFile) error {
	for _, f := range files {
		if !f.changed() {
			continue
		}
		if err := checkRemovalFile(f, "remove"); err != nil {
			return err
		}
	}
	return nil
}

// checkRemovalFile refuses a file that moved behind a symlink or changed
// since the plan was printed; command names the command to rerun.
func checkRemovalFile(f wireFile, command string) error {
	if f.root != "" {
		rel, err := filepath.Rel(f.root, f.path)
		if err != nil {
			return err
		}
		if err := wiringSafePath(f.root, filepath.ToSlash(rel)); err != nil {
			return err
		}
	}
	if err := initSafeFile(f.path); err != nil {
		return err
	}
	current, err := os.ReadFile(f.path)
	if (f.exists && (err != nil || !bytes.Equal(current, f.old))) || (!f.exists && !os.IsNotExist(err)) {
		return fmt.Errorf("%s changed after the plan was printed; rerun %s to review it", f.rel, command)
	}
	return nil
}

func applyRemoval(files []wireFile) error {
	if err := checkRemoval(files); err != nil {
		return err
	}
	for _, f := range files {
		if !f.changed() {
			continue
		}
		if err := checkRemovalFile(f, "remove"); err != nil {
			return err
		}
		if f.next != nil {
			if err := writeInitFile(f.path, f.old, f.next, f.exists); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(f.path); err != nil {
			return err
		}
		removeEmptyWiringParents(f.root, f.path)
	}
	return nil
}

// Prune only empty directories on the deleted file's path, stopping before
// the repository root. Cleanup is best-effort: user content always stays.
func removeEmptyWiringParents(root, path string) {
	if root == "" {
		return
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return
		}
		if err := wiringSafePath(root, filepath.ToSlash(rel)); err != nil {
			return
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}
