---
name: docs-change
description: Changing documentation without changing product behavior.
---

# docs-change

Docs describe the code as it is; every claim in them is checkable.

1. Write in the present tense: the docs say what the code does, not what was done to it.
2. Match the code: every command, flag, path, name and default the text names exists as written.
3. Run the commands in the doc, in order; a command that fails is a bug in the doc.
4. Check the cross-links hold: anchors, section numbers and file paths resolve where they point.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
