#!/bin/sh
# The keys allowed to connect come from the AUTHORIZED_KEYS secret and are
# rewritten on every start, so changing the secret and redeploying is enough.
set -eu
if [ -n "${AUTHORIZED_KEYS:-}" ]; then
    printf '%s\n' "$AUTHORIZED_KEYS" > "$SOUV__ssh__authorizedKeys"
    chmod 600 "$SOUV__ssh__authorizedKeys"
fi
exec souvenir "$@"
