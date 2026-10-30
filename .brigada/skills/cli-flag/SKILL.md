---
name: cli-flag
description: Adding or changing a command-line flag or argument.
---
<!-- brigada: version=0.0.0-dev+b995630cda52 digest=fbec6b35c47a5923661151f4327f42721246efa2de553a20cfbd523454eebca3 -->

# cli-flag

1. Register the flag on the command with its type and default, beside its
   siblings, using the command's own flag-adding helper.
2. Write its help text (the one-line abstract) and usage text (the long
   line with the value placeholder) in the command's existing voice.
3. Update the docs: the command's synopsis and the explanatory sentence
   under it, and any flag table that lists the command's flags.
4. Add a success test that runs the command with the flag and asserts the
   behavior it causes, and a usage-error test that runs it with the flag
   misspelled, missing, or given an invalid value and asserts the error.
5. State the compatibility in the summary: the default when the flag is
   absent, earlier spellings kept or removed, and any environment or
   config equivalent.
6. Check the sibling surfaces — the same behavior on the CLI, the API,
   the MCP tools and the UI — and add the flag to each that applies, or
   say in the summary why a surface does not take it.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
