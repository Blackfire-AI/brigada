---
name: dependency
description: Adding, upgrading, or removing a software dependency.
---

# dependency

1. Explain the need: what the new version fixes or what the removed
   dependency was doing, and what the transitive effects are.
2. Update the lockfile with the repository's own package command, and
   commit the manifest and the lockfile together.
3. Update the license and notices files the repository keeps, so the
   new dependency's license is recorded.
4. Check the new or changed versions for known vulnerabilities, and
   record the result in the summary.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
