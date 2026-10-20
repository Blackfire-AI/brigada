package main

// Codex's project config once carried the foreman bridge for this
// repository as the [mcp_servers.brigada] table of .codex/config.toml
// (design §4.6 item 1) — the same registration .mcp.json carried for
// Claude Code. Init no longer writes it; a rerun of init and brigada
// remove strip the table. The file is edited by hand, as the JSON removals
// are: only brigada's table changes, never the rest of the user's bytes,
// and a file brigada cannot edit safely is refused with an actionable
// error.

import (
	"fmt"
	"strings"
)

// codexServerTable is brigada's table name in Codex's config.
const codexServerTable = "mcp_servers.brigada"

func tomlDamaged(format string, args ...any) error {
	return fmt.Errorf("damaged or unexpected TOML: %s", fmt.Sprintf(format, args...))
}

// tomlNormalize strips the quoting from a header name or key the way a TOML
// reader would for simple segments: mcp_servers."brigada" and
// mcp_servers.brigada name the same thing. Anything more exotic stays as it
// is, which merely fails to match brigada's names.
func tomlNormalize(s string) string {
	var parts []string
	for _, seg := range strings.Split(s, ".") {
		seg = strings.TrimSpace(seg)
		if len(seg) >= 2 && (seg[0] == '"' || seg[0] == '\'') && seg[len(seg)-1] == seg[0] {
			seg = seg[1 : len(seg)-1]
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, ".")
}

// tomlHeader returns the normalized name of a table header line and whether
// it is one; [[array]] tables count, by their inner name.
func tomlHeader(line string) (string, bool) {
	t := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(t, "[") {
		return "", false
	}
	t = t[1:]
	if strings.HasPrefix(t, "[") {
		t = t[1:]
	}
	end := strings.IndexByte(t, ']')
	if end < 0 {
		return "", false
	}
	return tomlNormalize(t[:end]), true
}

// scanCodexConfig splits the document into lines and marks, per line, the
// normalized name of a table header and the normalized key of a key = value
// line. Deeper parsing is not attempted: a header-looking line inside a
// multi-line value would read as a header — an error Codex itself reports
// when it reads the file.
func scanCodexConfig(d []byte) (lines, headers, keys []string) {
	lines = strings.Split(string(d), "\n")
	headers = make([]string, len(lines))
	keys = make([]string, len(lines))
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if name, ok := tomlHeader(line); ok {
			headers[i] = name
			continue
		}
		if eq := strings.IndexByte(line, '='); eq > 0 {
			keys[i] = tomlNormalize(line[:eq])
		}
	}
	return lines, headers, keys
}

// codexServerSpans finds brigada's table: the line indexes of its header(s)
// and the line of the first value that defines mcp_servers.brigada outside a
// table header, which brigada refuses to touch.
func codexServerSpans(lines, headers, keys []string) (ours []int, inline int) {
	inline = -1
	table := ""
	for i := range lines {
		if headers[i] != "" {
			table = headers[i]
			if headers[i] == codexServerTable {
				ours = append(ours, i)
			}
			continue
		}
		k := keys[i]
		if k == "" {
			continue
		}
		if (table == "" && (k == "mcp_servers" || k == codexServerTable || strings.HasPrefix(k, codexServerTable+"."))) ||
			(table == "mcp_servers" && (k == "brigada" || strings.HasPrefix(k, "brigada."))) {
			inline = i
		}
	}
	return ours, inline
}

// removeCodexServer removes the [mcp_servers.brigada] table and the blank
// separator next to it; the file is deleted when nothing else is in it.
func removeCodexServer(d []byte, exists bool) ([]byte, string, error) {
	if !exists || len(strings.TrimSpace(string(d))) == 0 {
		return d, "unchanged", nil
	}
	lines, headers, keys := scanCodexConfig(d)
	ours, inline := codexServerSpans(lines, headers, keys)
	switch {
	case inline >= 0:
		return nil, "", tomlDamaged("line %d defines %s as a value, not a table; remove that entry yourself", inline+1, codexServerTable)
	case len(ours) > 1:
		return nil, "", tomlDamaged("duplicate [%s] tables at lines %d and %d; remove the extra one yourself", codexServerTable, ours[0]+1, ours[1]+1)
	case len(ours) == 0:
		return d, "unchanged", nil
	}
	h := ours[0]
	end := h + 1
	for end < len(lines) && headers[end] == "" {
		end++
	}
	start := h
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		// The body swallows every blank line up to the next header, so a
		// table in the middle takes its own separators with it. When the
		// table is the last content, the blank line before it was init's
		// separator and goes too.
		last := true
		for i := end; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) != "" {
				last = false
				break
			}
		}
		if last {
			start--
		}
	}
	next := append(append([]string{}, lines[:start]...), lines[end:]...)
	out := strings.Join(next, "\n")
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if strings.TrimSpace(out) == "" {
		return nil, "removed", nil
	}
	return []byte(out), "removed", nil
}
