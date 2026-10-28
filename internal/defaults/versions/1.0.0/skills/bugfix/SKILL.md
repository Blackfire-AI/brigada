---
name: bugfix
description: Correcting behavior that fails for a particular case.
---

# bugfix

A fix without a failing test is a story; write the test first.

1. Write the regression test and run it: it must fail before the fix, naming the failing case.
2. Diagnose the cause: read the code path, cite the line or commit that introduced the behavior, and check for sibling cases of the same cause.
3. Apply the fix, minimal and at the cause, not a guard at the symptom.
4. Run the regression test again — it passes after the fix — and the package's other tests.
5. Update the docs if documented behavior changed, and add the changelog entry.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
