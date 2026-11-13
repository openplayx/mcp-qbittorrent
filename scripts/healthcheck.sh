#!/usr/bin/env bash
# Check API liveness/readiness on the deploy host (or locally without args).
#   scripts/healthcheck.sh [<ssh-target> <remote-dir>]
set -euo pipefail
check='port=$(grep "^API_PORT=" .env | cut -d= -f2); bind=$(grep "^API_BIND=" .env | cut -d= -f2); bind=${bind:-127.0.0.1}
for i in $(seq 1 30); do
  if curl -fsS "http://$bind:$port/readyz" >/dev/null 2>&1; then curl -fsS "http://$bind:$port/healthz"; exit 0; fi
  sleep 2
done
echo "not ready:"; curl -sS "http://$bind:$port/readyz"; exit 1'
if [ $# -ge 2 ]; then
  ssh ${SSH_OPTS:-} "$1" "cd '$2/deploy' && $check"
else
  cd "$(dirname "$0")/../deploy" && bash -c "$check"
fi
