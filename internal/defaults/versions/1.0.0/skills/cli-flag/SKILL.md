---
name: cli-flag
description: Adding or changing a command-line flag or argument.
---

# cli-flag

A flag is one surface of a behavior: register it once and cover every place a flag shows, or the surfaces drift.

1. Register the flag on its command: name, shorthand if any, argument shape, default value, and its place among the sibling flags.
2. Write its help text — one line saying what it does and when to use it — and its usage text in the command's usage synopsis.
3. Update the docs: the flag in the command's synopsis and one sentence in its docs section, matching the help text.
4. Add a success test: run the command with the flag and assert the behavior it causes.
5. Add a usage-error test: a bad value or a missing argument is refused with the flag named and the usage shown.
6. Write the compatibility statement in the docs: the default the flag keeps, what an old script that omits it sees, and which flags it combines with.
7. Check the sibling surfaces — the API, the MCP tools and the UI — and add the flag to each one that exposes the command, or say in the summary why a surface does not take it.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
