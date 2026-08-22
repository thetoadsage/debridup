#!/bin/sh
set -eu

# Docker secrets are mounted root-readable. Open the secret before dropping
# privileges so the application itself never needs to run as root or receive
# encryption material through its environment or process arguments.
if [ -n "${DEBRIDUP_ENCRYPTION_KEY_FILE:-}" ]; then
  exec 3<"$DEBRIDUP_ENCRYPTION_KEY_FILE"
  export DEBRIDUP_ENCRYPTION_KEY_FD=3
  unset DEBRIDUP_ENCRYPTION_KEY
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
