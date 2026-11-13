#!/usr/bin/env bash
# Run the isolated Docker end-to-end suite against a real qbittorrent-nox.
#   scripts/e2e.sh            (keeps nothing; KEEP=1 leaves the stack up)
# Env: DOCKER (default docker), API_PORT (default 28780), SEEDER_PORT (28781).
#      E2E_PREBUILT=1 for hosts without Go: uses dist/bittorrent-server-linux-<arch>
#      and test/e2e/e2e.test, built elsewhere with
#        scripts/build.sh linux/<arch> && GOOS=linux GOARCH=<arch> go test -c -tags e2e -o test/e2e/e2e.test ./test/e2e
# Cleanup only removes this project's containers/networks and its work dir.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
DOCKER="${DOCKER:-docker}"
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-opx-bt-e2e}"
export API_PORT="${API_PORT:-28780}" SEEDER_PORT="${SEEDER_PORT:-28781}"
export PUID="$(id -u)" PGID="$(id -g)"
export WORK="$root/test/e2e/work"
# Variables are passed explicitly: "sudo docker" would drop the environment.
compose_env() {
  echo "COMPOSE_PROJECT_NAME=$COMPOSE_PROJECT_NAME API_PORT=$API_PORT SEEDER_PORT=$SEEDER_PORT PUID=$PUID PGID=$PGID WORK=$WORK SEEDER_PBKDF2=${SEEDER_PBKDF2:-unset} ENGINE_PBKDF2=${ENGINE_PBKDF2:-unset}"
}
dc_cmd() { echo "${DOCKER%docker}env $(compose_env) docker compose -f $root/test/e2e/compose.yaml"; }
compose() { $(dc_cmd) "$@"; }
cleanup() {
  if [ "${KEEP:-0}" != 1 ]; then
    compose down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT
compose down -v --remove-orphans >/dev/null 2>&1 || true
rm -rf "$WORK"
mkdir -p "$WORK"/{seed,seeder-config,engine-config,downloads,api-config,api-data}

case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; *) arch=arm64 ;; esac
if [ "${E2E_PREBUILT:-0}" = 1 ]; then
  [ -x "dist/bittorrent-server-linux-$arch" ] && [ -x test/e2e/e2e.test ] || { echo "prebuilt binaries missing" >&2; exit 1; }
  cp "dist/bittorrent-server-linux-$arch" "$WORK/bts"
else
  "$root/scripts/build.sh" "linux/$arch" >/dev/null
  go build -o "$WORK/bts" ./cmd/server
fi
$DOCKER build -q -t openplayx/bittorrent-engine:5.1.0-r0 deploy/engine >/dev/null
$DOCKER build -q --build-arg TARGETARCH="$arch" -t openplayx/bittorrent-server:e2e -f deploy/Dockerfile . >/dev/null

rand() { head -c 18 /dev/urandom | base64 | tr -d '/+='; }
export SEEDER_PASS="$(rand)" ENGINE_PASS="$(rand)"
export SEEDER_PBKDF2="$(printf '%s\n' "$SEEDER_PASS" | "$WORK/bts" engine-password-hash)"
export ENGINE_PBKDF2="$(printf '%s\n' "$ENGINE_PASS" | "$WORK/bts" engine-password-hash)"
token_line="$("$WORK/bts" new-token e2e-app)"
export E2E_TOKEN="$(echo "$token_line" | sed -n 's/^token: //p')"
token_sha="$(printf '%s' "$E2E_TOKEN" | { command -v sha256sum >/dev/null && sha256sum || shasum -a 256; } | cut -d' ' -f1)"
cat > "$WORK/api-config/config.json" <<JSON
{
  "listen": "0.0.0.0:8780", "data_dir": "/data", "secrets_file": "/etc/bittorrent-server/secrets.json",
  "public_name": "OpenPlayX BitTorrent e2e",
  "engine": {"url": "http://qbittorrent:8080", "supported_versions": ["v5.1.0"], "min_webapi_version": "2.11.0", "timeout_seconds": 5},
  "download_root": "/downloads",
  "destinations": [{"ref": "default", "name": "E2E", "sub_path": "e2e"}],
  "limits": {"max_active_tasks": 20, "max_task_bytes": 1073741824, "min_free_bytes": 1048576,
    "max_torrent_file_bytes": 1048576, "max_magnet_length": 4096, "max_download_rate": 52428800,
    "max_upload_rate": 52428800, "plan_ttl_seconds": 600, "input_ttl_seconds": 600, "requests_per_minute": 6000},
  "seeding": {"ratio_limit": -2, "seeding_time_minutes": -2},
  "network": {"allow_private_networks": true, "tracker_schemes": ["http"]},
  "files": {"content_access": true},
  "reconcile_interval_seconds": 1, "retention_days": 1
}
JSON
printf '{"api_tokens":[{"caller":"e2e-app","token_sha256":"%s"}],"engine":{"username":"engine","password":"%s"}}\n' "$token_sha" "$ENGINE_PASS" > "$WORK/api-config/secrets.json"
chmod 600 "$WORK/api-config/secrets.json"

compose up -d --quiet-pull
if [ "${KEEP:-0}" = 1 ]; then
  # Only for an inspected, kept stack; the work dir is gitignored and local.
  printf 'TORRENT_E2E_URL=http://127.0.0.1:%s\nTORRENT_E2E_TOKEN=%s\n' "$API_PORT" "$E2E_TOKEN" > "$WORK/e2e.env"
  chmod 600 "$WORK/e2e.env"
fi
export E2E_API="http://127.0.0.1:$API_PORT" E2E_SEEDER="http://127.0.0.1:$SEEDER_PORT"
export E2E_COMPOSE="$(dc_cmd)"
export E2E_SECRETS="$ENGINE_PASS,$SEEDER_PASS,$E2E_TOKEN"
set +e
if [ "${E2E_PREBUILT:-0}" = 1 ]; then
  (cd test/e2e && ./e2e.test -test.v -test.count=1 -test.timeout 15m) 2>&1 | tee "$WORK/e2e.log"
else
  go test -tags e2e -count=1 -timeout 15m -v ./test/e2e/ 2>&1 | tee "$WORK/e2e.log"
fi
status=${PIPESTATUS[0]}
set -e
if [ "$status" != 0 ]; then
  echo "--- api logs (tail)"; compose logs --tail 60 api || true
fi
mkdir -p "$root/test/e2e/reports"
cp "$WORK/e2e.log" "$root/test/e2e/reports/last-run.log" 2>/dev/null || true
[ -f "$WORK/report.json" ] && cp "$WORK/report.json" "$root/test/e2e/reports/last-run.json"
exit "$status"
