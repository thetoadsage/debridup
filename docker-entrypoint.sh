#!/bin/sh
set -eu

# Docker secrets are mounted root-readable. Read the secret before dropping
# privileges so the application itself never needs to run as root.
if [ -n "${DEBRIDUP_ENCRYPTION_KEY_FILE:-}" ]; then
  export DEBRIDUP_ENCRYPTION_KEY="$(cat "$DEBRIDUP_ENCRYPTION_KEY_FILE")"
  unset DEBRIDUP_ENCRYPTION_KEY_FILE
fi

run_uid="${PUID:-$(id -u debridup)}"
run_gid="${PGID:-$(id -g debridup)}"
case "$run_uid:$run_gid" in
  *[!0-9:]*|:*)
    echo "PUID and PGID must be numeric IDs" >&2
    exit 64
    ;;
esac

exec su-exec "$run_uid:$run_gid" /usr/local/bin/debridup
