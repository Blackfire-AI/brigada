# 1. Principles

1. **The foreman decides; the engine enforces.** Judgment (planning, answering, re-planning) is an LLM worker (the foreman, §4). Mechanics (queues, leases, eligibility, caps, routine retries) are deterministic Go code.
2. **The engine is the mailbox.** The foreman and the task workers never talk directly. Every message goes through the engine, and every message is kept (§9).
3. **No state lives only in a session.** Any session, the foreman's or a task worker's, can die at any moment; the engine and the plan file hold everything needed to continue.
4. **Prompts: templates plus verbatim data.** The engine composes a prompt only from fixed, versioned templates plus verbatim data (a task spec, review text, check output, a foreman answer). Verbatim data is always wrapped in explicit data delimiters, so one worker's output can't pose as instructions to the foreman or another worker. Anything that needs judgment goes to the foreman.

   **The one other input that is not data is a user message** (§4.2 item 3): the user's own words, sent through the client API, reach the foreman as a request to act on, in their own section and never inside an event's. Only a `client` or `admin` token can send one, which stops a worker or a foreman turn from *posting* one. Delimiters stop the rest: in the foreman's prompts the engine neutralizes the delimiter tags (`<event>`, `</event>`, `<user-message>`, `</user-message>`) inside any verbatim data it wraps, so no data can close its section and open another; the worker prompts' delimiters get the same treatment.
5. **Remote-ready** (product §5): clients only through the API, runners only through the lease protocol, tokens everywhere, repos by URL, self-contained state.
