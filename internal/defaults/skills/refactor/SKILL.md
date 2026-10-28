---
name: refactor
description: Restructuring code while preserving observable behavior.
---

# refactor

The existing tests are the proof the behavior survived; touch nothing else.

1. State the observable behavior preserved: no public API change — no new, removed or reshaped exports.
2. Move the code; keep the existing tests untouched, or the proof is gone.
3. Update docs only for renamed names, nowhere else.
4. Run the package's tests and the check the repo gives; the diff carries no behavior change.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
