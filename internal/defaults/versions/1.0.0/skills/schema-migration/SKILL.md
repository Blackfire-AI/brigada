---
name: schema-migration
description: Changing a persistent schema or data migration.
---

# schema-migration

A migration is applied once, in order, by the world; its number is that order.

1. Take the next migration number; never renumber an existing migration.
2. Write the migration to run once and leave a failed store exactly as it was.
3. Test on old data: build the store at the previous version, open it with the new binary, and assert the new state and the kept rows.
4. Document where state lives: the table and columns the migration adds, and the doc that names them.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
