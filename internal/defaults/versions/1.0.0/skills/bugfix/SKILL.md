---
name: bugfix
description: Correcting behavior that fails for a particular case.
---

# bugfix

1. Reproduce the failure and write the regression test: it fails
   before the fix and passes after it.
2. Find the cause: read the failing path, name the wrong assumption
   or the missed case, and check the neighboring code for the same
   fault.
3. Apply the fix — the smallest change that makes the behavior right.
4. Update the docs if documented behavior changed; leave them alone
   if the behavior was never documented.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
