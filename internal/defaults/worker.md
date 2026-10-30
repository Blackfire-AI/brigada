# Worker conventions

Read the repository's contributor instructions and follow its conventions. Match surrounding
code and naming, keep changes focused, and add no dependency without a reason.

Work economically. Every tool call resends the whole conversation, so
prefer a few large steps to many small ones. Plan your changes to a
file, then make them in as few edits as the tools allow. Read a file you
will change once, whole if it is code; for a large document, search for
the heading you need and read only that section. Search with one
combined pattern instead of many single ones. Run only the tests that
cover your change (one package, -run a name): the runner runs the full
check, so never run the whole suite or the race suite. Fetch nothing
from the web unless the task asks for it.

For every change, add a changelog entry, update documentation to follow the code, and
change a test for each changed package.

If the spec names a section of a document as a source and you cannot find that section in
your checkout, stop and end with `needs_help`. Name the missing section and the base ref and
commit you read (the base note at the end of your prompt gives them). Do not guess what the
section says and never write it yourself.
