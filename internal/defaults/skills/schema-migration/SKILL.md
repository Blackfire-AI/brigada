---
name: schema-migration
description: Changing a persistent schema or data migration.
---

# schema-migration

1. Take the next migration number in the sequence; never renumber or
   reuse an existing migration, whatever the temptation to tidy.
2. Write the migration so it runs once, in order, inside the same
   transaction that bumps the schema version.
3. Migrate the old data in the same migration — backfill, transform or
   seed — and test on old data: a store built from the migrations before
   yours, opened by the new code, must come up correct.
4. Document where the state lives: the table and column the migration
   touches, in the schema's own comments and the design's terms.
5. Never loosen or delete a check to make a test pass; if the check is
   wrong, change it in its own migration and say so in the summary.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
