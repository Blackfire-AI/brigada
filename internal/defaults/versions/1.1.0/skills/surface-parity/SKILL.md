---
name: surface-parity
description: Keeping equivalent behavior consistent across multiple interfaces.
---

# surface-parity

1. List the sibling surfaces the behavior belongs to among the CLI, the
   API, the MCP tools and the UI, and name the one this change starts
   from.
2. Add the behavior to each applicable surface, following that surface's
   own conventions for options, errors and output.
3. For a surface the behavior cannot reach, say why not in the summary —
   a technical reason, not silence.
4. Test each added surface the way that surface is tested, with the same
   expected behavior everywhere.
5. Keep names, defaults and error wording equivalent across the
   surfaces, so a user of two of them reads one feature.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
