#!/usr/bin/env bash
# Deploy to a Docker host over SSH into an isolated project directory.
#   scripts/deploy.sh <ssh-target> <remote-dir>
# Env: DOCKER (default "docker"; use "sudo docker" where needed),
#      SSH_OPTS (extra ssh options).
# The remote <dir>/deploy/.env and config/{config,secrets}.json must already
# exist (created once by the operator); this script never overwrites them,
# never touches other compose projects, and never prunes Docker globally.
set -euo pipefail
target="${1:?ssh target}" remote="${2:?remote dir}"
root="$(cd "$(dirname "$0")/.." && pwd)"
DOCKER="${DOCKER:-docker}"
ssh_() { ssh ${SSH_OPTS:-} "$target" "$@"; }
arch="$(ssh_ uname -m)"
case "$arch" in x86_64|amd64) goarch=amd64 ;; aarch64|arm64) goarch=arm64 ;; *) echo "unsupported arch $arch" >&2; exit 1 ;; esac
"$root/scripts/build.sh" "linux/$goarch" >/dev/null
version="$(cat "$root/dist/VERSION")"
ssh_ "mkdir -p '$remote/dist' '$remote/deploy/config' '$remote/deploy/state/api' '$remote/deploy/state/qbittorrent'"
ssh_ "test -f '$remote/deploy/.env' && test -f '$remote/deploy/config/config.json' && test -f '$remote/deploy/config/secrets.json'" || {
  echo "remote $remote/deploy/.env and config/{config,secrets}.json must exist first (see docs/operations.md)" >&2; exit 1; }
rsync -a --delete --exclude .env --exclude releases.log --exclude '*.bak' --exclude 'config/config.json' --exclude 'config/secrets.json' --exclude state \
  -e "ssh ${SSH_OPTS:-}" "$root/deploy/" "$target:$remote/deploy/"
rsync -a -e "ssh ${SSH_OPTS:-}" "$root/dist/bittorrent-server-linux-$goarch" "$root/dist/SHA256SUMS" "$root/dist/VERSION" "$target:$remote/dist/"
ssh_ "cd '$remote/deploy' && prev=\$(grep '^API_TAG=' .env | cut -d= -f2) && \
  sed -i.bak \"s/^API_TAG=.*/API_TAG=$version/\" .env && \
  echo \"\$(date -u +%FT%TZ) $version prev=\$prev\" >> releases.log && \
  $DOCKER compose --env-file .env build && $DOCKER compose --env-file .env up -d"
"$root/scripts/healthcheck.sh" "$target" "$remote"
echo "deployed $version"
