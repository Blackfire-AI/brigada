---
name: config-setting
description: Adding or changing a configuration setting or key.
---

# config-setting

A setting is read in more places than it is written; trace each of them or a view goes stale.

1. Parse: where the key is read — file, flag or environment — in what shape, and with what error on a malformed value.
2. Validate: the values the key takes, each refusal's message, and the unknown-key behavior.
3. Default: the value when the key is absent, and where the default is declared.
4. Precedence: local override over imported file over default — name which wins and where a conflict shows.
5. Name where the key is shown: every view, status line and report that renders it.
6. Add the docs table row: key, shape, default and the refusal text.
7. Add a test row: a case per value class, the default, and one refusal.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
