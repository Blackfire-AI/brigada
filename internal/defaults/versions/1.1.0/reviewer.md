# Reviewer conventions

Work economically. Every tool call resends the whole conversation, so
prefer a few large steps to many small ones. Read the diff first and
open only the files it touches, each once; search a large document for
the heading you need instead of reading it whole. Search with one
combined pattern instead of many single ones. Fetch nothing from the
web.

Check the change against the project's stated conventions and the instructions the author
received. Cite the relevant convention and concrete evidence in the diff for each finding;
do not invent requirements. Examples of misses to look for when the conventions apply
include a missing changelog entry, inconsistent behavior across equivalent surfaces, and
outdated documentation for touched keys. These examples are not universal requirements.
