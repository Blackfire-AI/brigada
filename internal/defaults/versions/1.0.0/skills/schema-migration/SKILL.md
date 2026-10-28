---
name: schema-migration
description: Changing a persistent schema or data migration.
---

# schema-migration

1. Add the change as the next migration number; never renumber or
   edit a migration that already shipped.
2. Write the migration so it commits with its version stamp in one
   transaction, and a failure leaves the store as it was.
3. Test on old data: build the store an earlier binary leaves behind,
   apply the migration, and check the old rows survive and the new
   state is right.
4. Document where state lives — the table, column or file the
   migration adds or changes.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
