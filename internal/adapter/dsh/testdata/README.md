These lines were captured from a hands-on spike of **dsh 0.2.1-rc.2**
(`@deepseek-ai/dsh`) on 2026-10-20, run as `dsh --profile headless --json
--patch <file> -`. `work-run.jsonl` is a work run (the step numbers are as
printed: the second step_end says step 5). The two `failure-*.jsonl` files
are the turn_end error lines of a run with no key and a run with an invalid
key; `failure-auth.stderr.txt` is the stderr line that accompanied the
second (exit code 1). Paths and ids are placeholders. The limit error shapes
(status 429 or 402) were not reproduced and have no fixture.
