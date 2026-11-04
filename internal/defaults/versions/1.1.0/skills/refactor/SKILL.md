---
name: refactor
description: Restructuring code while preserving observable behavior.
---

# refactor

1. Name the public API surface the code exposes — functions, types,
   flags, output, stored shapes — and carry it across unchanged: a
   refactor makes no public API change.
2. Restructure in small steps, each leaving the code building and the
   behavior identical; move code before changing it, one commit-sized
   step at a time.
3. Leave the existing tests untouched and passing; a test that must
   change to pass marks behavior change, not refactoring — stop and
   reconsider there.
4. Update the documentation only for renamed names the docs still spell
   the old way; add no new prose for moved code.
5. Add a changelog entry only when the rename or move is itself worth a
   line, and never claim a behavior change.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
