# 2. Processes

| Process | What | Local (launch) | Remote (backlog) |
|---|---|---|---|
| `brigada` (client) | CLI: attach, ask, status, history, stats, cancel, review, budget, recover | your terminal | any device |
| engine | API, store, rules, dispatcher, foreman wake-ups | background process, started on demand | a server |
| runner | leases jobs, runs agent CLIs, runs checks | same host, separate process | any host with logged-in CLIs |
| foreman session | a brigada-owned session: headless turns woken by events, your chat through `brigada attach` (§4.2), one transcript | headless turns started by the runner, the attach by you, both in the project's reading checkout under the state directory | same |
| worker session | a **worker**: one instance of an official CLI, a harness plus a model, running headless. A task worker runs one task; the foreman is a long-lived worker (§4) | started by the runner | same |

The client, engine, runner and the MCP bridge (`brigada mcp`, §12.2) are the one `brigada` binary in different roles. The harness CLIs are the vendors' own official binaries, which brigada drives headless or which host the bridge as a child process; they are never the `brigada` binary.
