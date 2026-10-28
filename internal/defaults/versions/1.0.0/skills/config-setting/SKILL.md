---
name: config-setting
description: Adding or changing a configuration setting or key.
---

# config-setting

1. Parse the setting's value where the configuration is read, with an
   error that names the key and the bad value.
2. Validate the value against the shapes the setting allows, and
   refuse what it must not take.
3. Define the default the setting falls back to, and its precedence:
   a local override, then the imported file, then the default.
4. Show the setting where the other settings are shown, with its
   effective value and source.
5. Add the setting's documentation table row.
6. Add a test row for each of parse, validation, default and
   precedence.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
