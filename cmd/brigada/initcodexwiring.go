package main

// Codex's project config: init registers the foreman bridge for this
// repository only, as the [mcp_servers.brigada] table of .codex/config.toml
// (design §4.6 item 1) — the same registration .mcp.json carries for Claude
// Code. Codex loads a project .codex/config.toml only once the folder is
// trusted (verified from Codex's documentation and history: project config
// since Codex 0.78.0, trust gating since 0.88.0), which is why init's last
// message for Codex says to trust the folder on the first codex run.
//
// The table is edited by hand, as the JSON merges are: only brigada's table
// changes, never the rest of the user's bytes, and a file brigada cannot
// edit safely is refused with an actionable error.

import (
	"bytes"
	"fmt"
	"strings"
)

// codexServerTable is brigada's table name in Codex's config.
const codexServerTable = "mcp_servers.brigada"

// codexServerBody renders brigada's table body at the given indent.
func codexServerBody(indent string) string {
	return indent + "command = \"brigada\"\n" +
		indent + "args = [\"mcp\", \"--foreman\"]\n"
}

// codexServerFile is the whole file init creates when nothing is there.
func codexServerFile() []byte {
	return []byte("[" + codexServerTable + "]\n" + codexServerBody(""))
}

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

// mergeCodexServer adds or replaces the [mcp_servers.brigada] table; every
// other byte stays. The same rules as mergeMCPServer, minus a TOML library.
func mergeCodexServer(d []byte, exists bool) ([]byte, string, error) {
	if !exists || len(bytes.TrimSpace(d)) == 0 {
		if exists {
			return codexServerFile(), "replaced", nil
		}
		return codexServerFile(), "created", nil
	}
	lines, headers, keys := scanCodexConfig(d)
	ours, inline := codexServerSpans(lines, headers, keys)
	switch {
	case inline >= 0:
		return nil, "", tomlDamaged("line %d defines %s as a value, not a table; remove that entry and rerun init", inline+1, codexServerTable)
	case len(ours) > 1:
		return nil, "", tomlDamaged("duplicate [%s] tables at lines %d and %d; remove the extra one and rerun init", codexServerTable, ours[0]+1, ours[1]+1)
	case len(ours) == 1:
		// The body runs to the next header; a header inside it means a
		// sub-table of ours, which stays. The blank lines that separate
		// the next table are the file's layout, not brigada's body, and
		// stay too — including the final newline's empty tail piece.
		h := ours[0]
		end := h + 1
		for end < len(lines) && headers[end] == "" {
			end++
		}
		for end > h+1 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		indent := lines[h][:len(lines[h])-len(strings.TrimLeft(lines[h], " \t"))]
		next := append([]string{}, lines[:h+1]...)
		next = append(next, strings.Split(strings.TrimSuffix(codexServerBody(indent), "\n"), "\n")...)
		next = append(next, lines[end:]...)
		out := strings.Join(next, "\n")
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		if out == string(d) {
			return d, "unchanged", nil
		}
		return []byte(out), "replaced", nil
	}
	// Nothing of ours is there: append the table at the end, behind a blank
	// separator when the file has other content.
	base := string(d)
	if !strings.HasSuffix(base, "\n") {
		base += "\n"
	}
	if !strings.HasSuffix(base, "\n\n") {
		base += "\n"
	}
	return []byte(base + "[" + codexServerTable + "]\n" + codexServerBody("")), "added", nil
}

// removeCodexServer removes the [mcp_servers.brigada] table and the blank
// separator next to it; the file is deleted when nothing else is in it.
func removeCodexServer(d []byte, exists bool) ([]byte, string, error) {
	if !exists || len(bytes.TrimSpace(d)) == 0 {
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

// planCodexWiring computes the Codex project-config change; like planWiring
// it reads files and writes nothing.
func planCodexWiring(root string) []wireFile {
	return planSteps(root, []wireStep{{wiringCodexFile, mergeCodexServer}})
}
