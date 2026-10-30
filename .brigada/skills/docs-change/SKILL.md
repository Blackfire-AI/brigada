---
name: docs-change
description: Changing documentation without changing product behavior.
---
<!-- brigada: version=0.0.0-dev+b995630cda52 digest=bc3fb5db7b131e4b7ad9dbaeb92c31f1a79f910d80c047dcf83ca8dd1c8ff2ff -->

# docs-change

1. Write in the present tense, describing what the code does now, and
   match the document's own voice and structure.
2. Verify every claim against the code: names, defaults, paths, flags
   and error wording, each read from the source it describes.
3. Run the commands the text names, or mark the ones you could not run,
   so no documented command is fiction.
4. Check that cross-links hold: anchors on the same page, files and
   headings the links name, and references to issues or sections that
   still exist.
5. Change no code: a wrong claim is fixed in the docs when the code is
   right, and in the code — its own task — when it is not.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
