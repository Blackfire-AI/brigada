---
name: cli-flag
description: Adding or changing a command-line flag or argument.
---

# cli-flag

1. Register the flag or argument on the command, following how the
   command's other flags are declared.
2. Write its help text and usage text, so `--help` and the usage error
   both name the flag and its value format.
3. Add the docs synopsis and its explanatory sentence in the
   command's documentation section.
4. Add a success test that runs the command with the flag and checks
   the behavior, and a usage-error test that passes a bad value and
   checks the message.
5. State the compatibility impact: a new flag is additive; a changed
   or removed one is a break, and the statement says which.
6. Check the sibling surfaces — CLI, API, MCP and UI — and add the
   behavior to each one it applies to, or say in the summary why not.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
