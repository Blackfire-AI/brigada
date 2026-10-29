package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ProjectConfigDefaults proposes init's defaults. Existing settings, comments
// and spelling survive; the caller shows the resulting diff and asks before
// writing. Empty input creates the minimal schema-1 file, with gate_push auto
// spelled out; an empty check omits or removes default_check so the project
// setting is unset.
func ProjectConfigDefaults(content []byte, repo, check, tools string) ([]byte, error) {
	if len(content) == 0 {
		content = []byte(fmt.Sprintf("schema: %d\nrepo: %s\n%s: %s\n", ProjectConfigSchema, yamlString(repo), SettingGatePush, GateAuto))
	}
	cfg, err := ParseProjectConfig(content)
	if err != nil {
		return nil, err
	}
	if cfg.Repo != repo {
		return nil, configErr("repo %q is not this repository's origin %q", cfg.Repo, repo)
	}
	lines := strings.Split(string(content), "\n")
	for _, item := range []struct{ key, value string }{{SettingDefaultCheck, check}, {SettingWorkerTools, tools}} {
		if err := ValidateSettingValue(item.key, item.value); err != nil {
			return nil, err
		}
		if item.key == SettingDefaultCheck && item.value == "" {
			for i, line := range lines {
				if strings.HasPrefix(line, item.key+":") {
					lines = append(lines[:i], lines[i+1:]...)
					break
				}
			}
			continue
		}
		var old string
		if json.Unmarshal(cfg.Values[item.key], &old) == nil && old == item.value {
			continue
		}
		replaced := false
		for i, line := range lines {
			if strings.HasPrefix(line, item.key+":") {
				plain := stripYAMLComment(line)
				comment := strings.TrimPrefix(line, plain)
				suffix := ""
				if comment != "" {
					suffix = " " + comment
				}
				lines[i] = item.key + ": " + yamlString(item.value) + suffix
				replaced = true
				break
			}
		}
		if !replaced {
			if lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			lines = append(lines, item.key+": "+yamlString(item.value), "")
		}
	}
	out := []byte(strings.Join(lines, "\n"))
	_, err = ParseProjectConfig(out)
	return out, err
}

// ProjectConfigGuidance adds what init's defaults need to a project.yaml
// (§5.9, §5.10): the instructions map naming the four role files at their
// default paths, and skills_dir. A key the file already has, and the old
// instructions_file alias, are left as they are; a partial instructions
// map gains the roles it lacks.
func ProjectConfigGuidance(content []byte) ([]byte, error) {
	cfg, err := ParseProjectConfig(content)
	if err != nil {
		return nil, err
	}
	var add strings.Builder
	if cfg.RoleFiles != nil {
		// A partial map keeps its paths; the roles it leaves out join it
		// at their default paths, so all four files are generated.
		var missing []string
		for _, role := range Roles {
			if _, ok := cfg.RoleFiles[role]; !ok {
				missing = append(missing, "  "+role+": "+DefaultRoleFile(role))
			}
		}
		if len(missing) > 0 {
			lines := strings.Split(string(content), "\n")
			at, last := -1, -1
			for i, l := range lines {
				switch {
				case at < 0:
					if strings.TrimSpace(stripYAMLComment(l)) == configKeyInstructions+":" && !strings.HasPrefix(l, " ") {
						at, last = i, i
					}
				case strings.HasPrefix(l, " "):
					if strings.TrimSpace(stripYAMLComment(l)) != "" {
						last = i
					}
				case strings.TrimSpace(l) != "":
					goto found
				}
			}
		found:
			if at < 0 {
				return nil, configErr("cannot find the %s map to complete", configKeyInstructions)
			}
			lines = append(lines[:last+1], append(missing, lines[last+1:]...)...)
			content = []byte(strings.Join(lines, "\n"))
			if _, err := ParseProjectConfig(content); err != nil {
				return nil, err
			}
		}
	}
	if cfg.RoleFiles == nil && cfg.InstructionsFile == "" {
		add.WriteString(configKeyInstructions + ":\n")
		for _, role := range Roles {
			add.WriteString("  " + role + ": " + DefaultRoleFile(role) + "\n")
		}
	}
	if cfg.SkillsDir == "" {
		add.WriteString(configKeySkillsDir + ": " + DefaultSkillsDir + "\n")
	}
	if add.Len() == 0 {
		return content, nil
	}
	out := string(content)
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	res := []byte(out + add.String())
	_, err = ParseProjectConfig(res)
	return res, err
}

func yamlString(s string) string { b, _ := json.Marshal(s); return string(b) }
