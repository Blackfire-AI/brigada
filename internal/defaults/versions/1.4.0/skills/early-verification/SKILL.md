---
name: early-verification
description: Plans that change rendered copy or UI, verified on representative surfaces before the full rollout.
---

# early-verification

1. Finish the first implementation batch, then stop and verify before
   the full rollout; a defect found after the rollout costs every
   surface, one found now costs the batch.
2. Pick representative surfaces and check each in every target language
   and viewport class: one long token, one wide table, one pluralized
   or monetary value expose whole classes of problems.
3. Look for overflow and truncation, wrapping, plural and number
   forms, currency and date formats, and missing or untranslated keys.
4. Report the findings as one batch, not one at a time, so the rollout
   fixes the classes once.
5. Carry the fixes into the rollout, then verify again on the surfaces
   that failed.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
