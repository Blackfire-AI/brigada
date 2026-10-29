---
name: bugfix
description: Correcting behavior that fails for a particular case.
---

# bugfix

1. Reproduce the bug with a new test that fails on the current code, and
   name the test after the broken behavior.
2. Find the cause: read the failing path, cite the file and line that
   are wrong, and check the neighboring code for the same fault.
3. Apply the fix at the cause, not at the symptom; keep the change
   minimal and match the surrounding style.
4. Run the new test around the fix: it must fail before the fix and pass
   after it. Run the package's other tests too.
5. Update the documentation when the fix changes documented behavior,
   and add a changelog entry when the fix is user-visible.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
