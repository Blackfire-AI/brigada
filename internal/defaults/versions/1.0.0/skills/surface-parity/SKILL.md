---
name: surface-parity
description: Keeping equivalent behavior consistent across multiple interfaces.
---

# surface-parity

One behavior, several interfaces: each sibling needs the change, or a recorded reason it does not take it.

1. List the siblings among CLI, API, MCP and UI that expose this behavior, and how each names it.
2. Add the behavior to each sibling in its own idiom: flags for the CLI, fields for the API, tools for MCP, controls for the UI.
3. For every sibling that does not take the change, say why not in the summary; silence is drift.
4. Test each changed sibling through its own surface, and keep the refusal wording equivalent across them.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
