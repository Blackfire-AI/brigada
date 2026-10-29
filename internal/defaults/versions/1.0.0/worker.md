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

A spec may name a docs section as its source. When your checkout does not
carry that section — a wrong base, a renamed heading, a typo in the spec —
do not guess and never write the section yourself: stop with needs_help and
name the missing section and the base ref and commit your prompt's base
note carries.
