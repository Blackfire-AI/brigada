package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ProjectConfigDefaults proposes only init's two defaults. Existing settings,
// comments and spelling survive; the caller shows the resulting diff and asks
// before writing. Empty input creates the minimal schema-1 file; an empty
// check omits or removes default_check so the project setting is unset.
func ProjectConfigDefaults(content []byte, repo, check, tools string) ([]byte, error) {
	if len(content) == 0 {
		content = []byte(fmt.Sprintf("schema: %d\nrepo: %s\n", ProjectConfigSchema, yamlString(repo)))
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
func yamlString(s string) string { b, _ := json.Marshal(s); return string(b) }
