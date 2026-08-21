#!/bin/sh
set -eu

# Docker secrets are mounted root-readable. Read the secret before dropping
# privileges so the application itself never needs to run as root.
if [ -n "${DEBRIDUP_ENCRYPTION_KEY_FILE:-}" ]; then
  export DEBRIDUP_ENCRYPTION_KEY="$(cat "$DEBRIDUP_ENCRYPTION_KEY_FILE")"
  unset DEBRIDUP_ENCRYPTION_KEY_FILE
fi

exec su-exec debridup /usr/local/bin/debridup
