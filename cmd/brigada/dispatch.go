package main

import (
	"context"
	"fmt"
	"io"
	"slices"
)

var projectCommands = map[string]command{
	"foreman": {name: "foreman", description: "Show or choose the foreman harness, model and effort.", run: runProjectForeman},
	"list":    {name: "list", description: "List projects, active plans and what needs you.", run: runProjectList},
	"set":     {name: "set", description: "Set project settings with key=value pairs.", run: projectSettingsCommand("set")},
	"show":    {name: "show", description: "Show project settings and their sources.", run: projectSettingsCommand("show")},
	"import":  {name: "import", description: "Import .brigada/project.yaml settings.", run: runProjectImport},
}

var agentCommands = map[string]command{
	"reset": {name: "reset", description: "Clear a broken or cooling agent.", run: runAgentReset},
}

var planCommands = map[string]command{
	"approve": {name: "approve", description: "Approve a plan and queue its tasks.", run: planDecisionCommand("approve")},
	"reject":  {name: "reject", description: "Reject a plan and cancel its tasks.", run: planDecisionCommand("reject")},
}

var taskCommands = map[string]command{
	"submit": {name: "submit", description: "Submit one task directly, without the foreman.", run: runTaskSubmit},
}

func projectSettingsCommand(verb string) func(context.Context, []string, io.Writer, io.Writer) int {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
		return runProjectSettings(ctx, verb, args, stdout, stderr)
	}
}

func planDecisionCommand(verb string) func(context.Context, []string, io.Writer, io.Writer) int {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
		return runPlanDecision(ctx, verb, args, stdout, stderr)
	}
}

// dispatch uses the same subcommand table for execution and help.
func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer, name, missing, want string, subcommands map[string]command) int {
	stderr = commandStderr(stderr, name)
	if len(args) == 0 {
		return usagef(stderr, "%s", missing)
	}
	names := make([]string, 0, len(subcommands))
	for sub := range subcommands {
		names = append(names, sub)
	}
	slices.Sort(names)
	switch args[0] {
	case "-h", "--help", "-help", "help":
		fmt.Fprintf(stdout, "Usage: brigada %s <subcommand> [arguments]\n\nSubcommands:\n", name)
		for _, sub := range names {
			fmt.Fprintf(stdout, "  %-10s %s\n", sub, subcommands[sub].description)
		}
		return 0
	}
	if cmd, ok := subcommands[args[0]]; ok {
		return cmd.run(ctx, args[1:], stdout, stderr)
	}
	return usagef(stderr, "unknown %s subcommand %q; want %s", name, args[0], want)
}
