---
name: config-setting
description: Adding or changing a configuration setting or key.
---

# config-setting

1. Parse: name where the value is read (file, flag or environment) and
   the shape it arrives in; reuse the parser the neighboring settings
   use.
2. Validate: the legal values, the refusal for each illegal one, and any
   normalization (trim, case) applied before validation.
3. Default: what an unset value resolves to, and where that default
   lives in the code.
4. Precedence: write the rule in one sentence — flag over environment
   over file over default — including any per-project or per-repo scope
   that intervenes.
5. Where it is shown: the status or show output that names the setting,
   and the documentation table row for it (key, values, default).
6. Add a test row: a parse of each legal value, a refusal of an illegal
   one, and the default when the setting is unset.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
