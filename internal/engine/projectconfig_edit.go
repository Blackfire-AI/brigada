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
// (§5.9, §5.10): the instructions map naming the role files at their
// default paths, and skills_dir. A file with no instructions map at all
// is first generation: it gains the whole map — or, when it keeps the
// 1.0 instructions_file, the three other roles only, the worker staying
// on its alias, for the file refuses naming both keys. Once the map
// exists a missing key is the user's choice: nothing is added back, so
// dropping a key reaches not configured at the next import, and init
// says so with the way to restore it (ProjectConfigRoleKey, the engine
// half of `brigada init --take-default <role>`). A key the map has is
// left as it is, and so is skills_dir.
func ProjectConfigGuidance(content []byte) ([]byte, error) {
	cfg, err := ParseProjectConfig(content)
	if err != nil {
		return nil, err
	}
	var add strings.Builder
	switch {
	case cfg.RoleFiles != nil:
		// The map exists: its missing keys were left out on purpose.
	case cfg.InstructionsFile != "":
		add.WriteString(configKeyInstructions + ":\n")
		for _, role := range RoleKeys {
			if role == RoleWorker {
				continue // instructions_file is the worker's key (§5.9)
			}
			add.WriteString("  " + role + ": " + DefaultRoleFile(role) + "\n")
		}
	default:
		add.WriteString(configKeyInstructions + ":\n")
		for _, role := range RoleKeys {
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
	if _, err := ParseProjectConfig(res); err != nil {
		return nil, err
	}
	return res, nil
}

// ProjectConfigRoleKey adds one role's key to a project.yaml's
// instructions map at the role's default path: the restore path of a
// role deliberately left out (§5.9), which `brigada init --take-default
// <role>` takes and init's plan names. A key the map already has keeps
// its path, and the worker stays on the instructions_file alias when the
// file carries one — a map key beside it is refused, as the file itself
// refuses both.
func ProjectConfigRoleKey(content []byte, role string) ([]byte, error) {
	if !slicesContains(RoleKeys, role) {
		return nil, configErr("unknown role %q; valid roles: %s", role, strings.Join(RoleKeys, ", "))
	}
	cfg, err := ParseProjectConfig(content)
	if err != nil {
		return nil, err
	}
	if _, ok := cfg.RoleFiles[role]; ok {
		return content, nil
	}
	if role == RoleWorker && cfg.InstructionsFile != "" {
		// The alias is the worker's key; adding the map's beside it
		// would name the same text twice (§5.9).
		return content, nil
	}
	line := "  " + role + ": " + DefaultRoleFile(role)
	if cfg.RoleFiles == nil {
		out := string(content)
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		res := []byte(out + configKeyInstructions + ":\n" + line + "\n")
		if _, err := ParseProjectConfig(res); err != nil {
			return nil, err
		}
		return res, nil
	}
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
	lines = append(lines[:last+1], append([]string{line}, lines[last+1:]...)...)
	res := []byte(strings.Join(lines, "\n"))
	if _, err := ParseProjectConfig(res); err != nil {
		return nil, err
	}
	return res, nil
}

func yamlString(s string) string { b, _ := json.Marshal(s); return string(b) }
