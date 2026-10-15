These JSONL files were recorded from the installed **opencode 1.19.0** CLI
on 2026-10-15. The CLI ran against `provider.mjs`, an offline AI SDK provider:
normal mode requests the real `read` tool, then returns the final text; error
modes throw SDK API errors with status 429 and 401. The CLI, rather than the
adapter tests, constructs every JSON envelope, part, and serialized error.
Provider error wording is controlled test input, not a claim about any vendor's
current error phrasing. No credentials, network requests, or `--auto` are used.

`record.py` regenerates all three files; run it from the worktree root with
opencode 1.19.0 on PATH. Session ids and absolute worktree paths are normalized;
part/message ids and timestamps remain recorded values. An FSEvents warning on
stderr in the sandbox is unrelated to the JSON output and is not a failure event.

The wire envelope is specified by the tagged upstream
[run.ts](https://github.com/anomalyco/opencode/blob/v1.19.0/packages/opencode/src/cli/cmd/run.ts),
with nested payloads in the generated
[SDK types](https://github.com/anomalyco/opencode/blob/v1.19.0/packages/sdk/js/src/v2/gen/types.gen.ts).
This CLI emits `tool_use` only on completion or tool error, not at tool start.
The inline running-state unit test exercises the documented ToolState schema;
it is deliberately not presented as a recorded CLI event. Step starts identify
sessions, not tools. Provider failures can be the only event: their envelope's
session id must survive alongside the error for write-back.
