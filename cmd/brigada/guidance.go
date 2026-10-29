package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/Blackfire-AI/brigada/internal/api"
	"github.com/Blackfire-AI/brigada/internal/engine"
)

// localProjectFile reads the working directory's .brigada/project.yaml, if
// there is one that parses: the repo root and its config. The digests the
// client reports come from currentFileDigests, the same ones the import
// computes (§5.9, §5.10).
func localProjectFile() (string, engine.ProjectConfig, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", engine.ProjectConfig{}, false
	}
	root, err := gitRoot(dir)
	if err != nil {
		return "", engine.ProjectConfig{}, false
	}
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(engine.ProjectConfigPath)))
	if err != nil {
		return "", engine.ProjectConfig{}, false
	}
	cfg, err := engine.ParseProjectConfig(content)
	if err != nil {
		return "", engine.ProjectConfig{}, false
	}
	return root, cfg, true
}

// skillDelta compares the skills on disk with the imported ones: the names
// added to skills_dir and removed from it since the import. A changed skill
// is the engine's comparison of digests, not this one. ok is false when
// the directory cannot be read, so nothing is called removed by mistake.
func skillDelta(st api.Status, project int64, cfg engine.ProjectConfig, root string) (added, removed []string, err error) {
	if cfg.SkillsDir == "" {
		return nil, nil, nil
	}
	disk, err := readSkills(root, cfg.SkillsDir)
	if err != nil {
		return nil, nil, err
	}
	onDisk := map[string]bool{}
	for _, s := range disk {
		onDisk[s.Name] = true
	}
	imported := map[string]bool{}
	for _, s := range st.Skills {
		if s.ProjectID != project {
			continue
		}
		imported[s.Name] = true
		if !onDisk[s.Name] {
			removed = append(removed, s.Name)
		}
	}
	for name := range onDisk {
		if !imported[name] {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed, nil
}

// guidanceChangeLines words every role file and skill that differs from the
// last import (§5.9, §5.10): changed, added or removed. Nothing stored
// changes until the next explicit import.
func guidanceChangeLines(st api.Status, project int64, cfg engine.ProjectConfig, root string) []string {
	var lines []string
	for _, r := range st.RoleInstructions {
		if r.ProjectID == project && r.ChangedSinceImport != nil && *r.ChangedSinceImport {
			lines = append(lines, fmt.Sprintf("instructions.%s %s: changed since the last import; brigada project import applies it", r.Role, cfg.RoleFiles[r.Role]))
		}
	}
	for _, s := range st.Skills {
		if s.ProjectID == project && s.ChangedSinceImport != nil && *s.ChangedSinceImport {
			lines = append(lines, fmt.Sprintf("skill %s: changed since the last import; brigada project import applies it", s.Name))
		}
	}
	added, removed, err := skillDelta(st, project, cfg, root)
	if err != nil {
		return append(lines, fmt.Sprintf("skills_dir %s: %v", cfg.SkillsDir, err))
	}
	for _, n := range added {
		lines = append(lines, fmt.Sprintf("skill %s: added since the last import; brigada project import applies it", n))
	}
	for _, n := range removed {
		lines = append(lines, fmt.Sprintf("skill %s: removed since the last import; brigada project import applies it", n))
	}
	return lines
}

// printGuidance is `project show`'s per-role block and skills list
// (§5.9): each role's state, file, size, digest, the version its default
// was generated from, whether it was edited, and whether its file changed
// since import; then each imported skill and what changed on disk. The
// comparisons need the working directory's project.yaml and say nothing
// without it.
func printGuidance(ctx context.Context, client *api.Client, stdout io.Writer, project int64) {
	if project == 0 {
		return
	}
	root, cfg, hasCfg := localProjectFile()
	var d api.FileDigests
	if hasCfg {
		d = currentFileDigests(root, cfg)
	}
	st, err := client.StatusWithFileDigests(ctx, project, d)
	if err != nil {
		return
	}
	for _, r := range st.RoleInstructions {
		if r.ProjectID != project {
			continue
		}
		file := "none"
		if p, ok := cfg.RoleFiles[r.Role]; ok {
			file = p
		}
		line := fmt.Sprintf("instructions.%s: %s, file %s", r.Role, r.State, file)
		if r.State != engine.RoleStateNotConfigured {
			line += fmt.Sprintf(", %d bytes, sha256 %s", r.Bytes, r.Digest)
		}
		if r.GeneratedFrom != "" {
			line += ", generated from " + r.GeneratedFrom
		}
		if r.Edited != nil {
			line += fmt.Sprintf(", edited %t", *r.Edited)
		}
		if r.ChangedSinceImport != nil {
			line += fmt.Sprintf(", changed since import %t", *r.ChangedSinceImport)
		}
		fmt.Fprintln(stdout, line)
	}
	skills := 0
	for _, s := range st.Skills {
		if s.ProjectID != project {
			continue
		}
		skills++
		line := fmt.Sprintf("skill %s: %s, sha256 %s", s.Name, s.Description, s.Digest)
		if s.ChangedSinceImport != nil {
			line += fmt.Sprintf(", changed since import %t", *s.ChangedSinceImport)
		}
		fmt.Fprintln(stdout, line)
	}
	if skills == 0 {
		fmt.Fprintln(stdout, "skills: none imported")
	}
	if hasCfg {
		added, removed, err := skillDelta(st, project, cfg, root)
		if err == nil {
			for _, n := range added {
				fmt.Fprintf(stdout, "skill %s: added since the last import\n", n)
			}
			for _, n := range removed {
				fmt.Fprintf(stdout, "skill %s: removed since the last import\n", n)
			}
		}
	}
}
