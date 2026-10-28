---
name: surface-parity
description: Keeping equivalent behavior consistent across multiple interfaces.
---

# surface-parity

1. List the sibling surfaces the behavior lives on — CLI, API, MCP and
   UI — whichever the repository ships.
2. Read each sibling's code for the behavior, and judge which of them
   the change applies to.
3. Add the behavior to each applicable surface, matching each one's
   conventions for flags, names, errors and tests.
4. Name in the summary any surface the change was not added to, and
   say why not.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
