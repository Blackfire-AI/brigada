---
name: refactor
description: Restructuring code while preserving observable behavior.
---

# refactor

1. Read the code's current behavior and name the public API that must
   not change: exported names, flags, output, stored state.
2. Restructure in steps that each leave the build green — move code
   before changing it.
3. Keep the existing tests untouched: they are the proof the behavior
   is preserved, and a test that must change names the change to
   justify in the summary.
4. Update the docs only for renamed names, and keep them otherwise as
   they were.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
