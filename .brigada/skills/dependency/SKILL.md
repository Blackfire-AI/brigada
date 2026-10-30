---
name: dependency
description: Adding, upgrading, or removing a software dependency.
---
<!-- brigada: version=0.0.0-dev+b995630cda52 digest=98ab0183699b9f06d92a6a0246401a19e420bbce1d6a91a056a44a71b32870ec -->

# dependency

1. State the need: what the new or changed dependency is for, and why an
   existing one or the standard library does not cover it.
2. Update the lockfile (and the manifest) with the exact version, and
   record any transitive change it pulls in.
3. Check the license of everything added: compatible with the project's,
   and reflected in the license notices (NOTICE, THIRD_PARTY_NOTICES or
   the project's equivalent).
4. Check for known vulnerabilities in the new versions, with the
   project's scanner or the advisory database, and record the result.
5. Run the package's tests and note any transitive effect on imports or
   behavior that the change causes.

If your tools can run the relevant tests, commands, or checks, do it; otherwise verify by inspection and say so in the summary.
