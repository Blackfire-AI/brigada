package api

import (
	"fmt"
	"strings"
)

// StatusSections renders one status answer's plans, foreman report and
// "needs you" sections (§6.1, §6.2, §3, §10) as plain text, after the
// pid/uptime and jobs lines: the `plans:` lines — one per plan, its
// tasks indented beneath it — then the `foreman report:` block — the
// latest report's time and first lines, the project named — then the
// `needs you:` queue, "nothing" when it is empty. The client's status
// command and the MCP bridge's status tool share this rendering, so a
// user and the foreman read the same lines; a task's branch rides in
// brackets only once it has one.
func StatusSections(st Status) string {
	var lines []string
	if len(st.Plans) == 0 {
		lines = append(lines, "plans: none")
	} else {
		lines = append(lines, "plans:")
		for _, p := range st.Plans {
			lines = append(lines, fmt.Sprintf("#%d %s %s", p.ID, p.State, p.Title))
			for _, t := range p.Tasks {
				line := fmt.Sprintf("  task #%d %s %s", t.ID, t.State, t.Title)
				if t.Branch != "" {
					line += " [" + t.Branch + "]"
				}
				lines = append(lines, line)
			}
		}
	}
	if st.Report == nil {
		lines = append(lines, "foreman report: none")
	} else {
		lines = append(lines, "foreman report:")
		for i, ln := range strings.Split(st.Report.Text, "\n") {
			if i == 0 {
				ln = fmt.Sprintf("%s project #%d: %s", st.Report.CreatedAt, st.Report.ProjectID, ln)
			} else if ln != "" {
				ln = "  " + ln
			}
			lines = append(lines, ln)
		}
	}
	if len(st.NeedsYou) == 0 {
		lines = append(lines, "needs you: nothing")
	} else {
		lines = append(lines, "needs you:")
		for _, n := range st.NeedsYou {
			line := fmt.Sprintf("%s #%d %s", n.Kind, n.ID, n.State)
			if n.Reason != "" {
				line += ": " + n.Reason
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}
