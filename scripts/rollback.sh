#!/usr/bin/env bash
# Roll the API back to the previous image tag recorded in releases.log.
# The engine image is pinned separately and not changed here.
#   scripts/rollback.sh <ssh-target> <remote-dir>
set -euo pipefail
target="${1:?ssh target}" remote="${2:?remote dir}"
DOCKER="${DOCKER:-docker}"
ssh ${SSH_OPTS:-} "$target" "cd '$remote/deploy' && prev=\$(tail -1 releases.log | sed -n 's/.*prev=//p') && \
  test -n \"\$prev\" && $DOCKER image inspect openplayx/bittorrent-server:\$prev >/dev/null && \
  sed -i.bak \"s/^API_TAG=.*/API_TAG=\$prev/\" .env && $DOCKER compose --env-file .env up -d api && \
  echo \"\$(date -u +%FT%TZ) rollback to \$prev\" >> releases.log"
"$(dirname "$0")/healthcheck.sh" "$target" "$remote"
