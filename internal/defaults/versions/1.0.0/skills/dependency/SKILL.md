---
name: dependency
description: Adding, upgrading, or removing a software dependency.
---

# dependency

A dependency change is a supply-chain change: the graph, its licenses and its holes all move.

1. State the need and the compatibility check: the version constraint, the transitive effects, and what else pins the dependency.
2. Update the lockfile — go.mod and go.sum, or the language's lockfile — together with the imports in one change.
3. Check the license of what ships in, and update the license notices (LICENSE, NOTICE, THIRD_PARTY_NOTICES) for a new dependency.
4. Run the vulnerability check over the new dependency graph (govulncheck or the language's equivalent) and record the result.
5. Run the package tests and add the changelog entry.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
