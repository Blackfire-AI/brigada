#!/bin/sh

# Regenerates THIRD_PARTY_NOTICES.md at the repository root from the real
# dependency tree of ./cmd/brigada, using go-licenses v1.6.0. The first
# run may need network to fetch the tool's own modules; afterwards it
# works from the module cache alone. It exits non-zero if any licence is
# not compatible with brigada's Apache-2.0 licence.
#
#     sh internal/tools/licenses/regenerate.sh

set -eu
cd "$(dirname "$0")"
exec go run . > ../../../THIRD_PARTY_NOTICES.md
