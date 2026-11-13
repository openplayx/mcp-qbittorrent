# 21177 acceptance

`21177` is the test deployment target; the user confirmed it is the LAN
test machine `211`.

| Item | Value |
|---|---|
| Reachable address / SSH target | `zyl@192.168.1.211` |
| Account, sudo for Docker | `zyl` (uid/gid 1000), Docker via `sudo` |
| CPU architecture / OS | x86_64, Ubuntu 26.04.1 LTS |
| Docker + compose versions | 29.9.1 / 5.6.0 |
| Free disk on the download root | ~32 GB (disk 97% used) → `max_task_bytes` 5 GiB, `min_free_bytes` 10 GiB |
| Existing services / ports in use | utorrent (6881, 8080), torrent-search (18790), source-test stack, Jellyfin/Plex, e2b, rustfs — untouched |

## Deployment

1. Choose an unused project name (e.g. `openplayx-bt-21177`), directory,
   API port, peer port and WebUI port. Do not reuse App/Relay/Home ports.
2. Follow `docs/operations.md` one-time setup, then
   `DOCKER="sudo docker" scripts/deploy.sh <ssh-target> <dir>`.
3. Record the API entry address after deployment.

## Acceptance run

The suite `scripts/e2e.sh` runs on the target host itself (it needs Docker
and Go): copy the repository, then `DOCKER="sudo docker" scripts/e2e.sh`.
It uses its own compose project (`opx-bt-e2e`), an internal network without
internet access, generated fixtures with recorded SHA-256 and throwaway
credentials, and removes only its own containers and work directory.
Results: `test/e2e/reports/last-run.{log,json}`.

Then, against the deployed instance (not the e2e stack):

- [ ] `/healthz`, `/readyz`, `/docs`, `/v1/capabilities` with the real token; wrong token → 401
- [ ] App: configure the service → upload a self-made `.torrent` → assistant adds it → progress card → pause/resume → remove (files kept)
- [ ] App: remove with file deletion enabled only in a test destination
- [ ] Engine restart and host reboot: tasks and states recovered
- [ ] Host offline: App keeps the configuration and tasks; Mac CI unaffected

Record: business commit, image digests (`docker image inspect`), engine/API
versions, host, ports, cases and checksums. Keep failures' sanitized logs.
Cleanup only touches this project's containers, volumes and test files.

## Acceptance record — 2026-11-13

Deployment: project `openplayx-bittorrent` in `/home/zyl/openplayx-bittorrent`
(`DOCKER="sudo docker" scripts/deploy.sh zyl@192.168.1.211 /home/zyl/openplayx-bittorrent`).

| Item | Value |
|---|---|
| API entry | `http://192.168.1.211:18780` (LAN only, no TLS) |
| Engine WebUI | `127.0.0.1:18781` on the host (SSH forward to the same port) |
| Peer port | 46881 tcp+udp |
| Download root | `/home/zyl/openplayx-bittorrent/downloads` (destination `default`) |
| API image | `openplayx/bittorrent-server:dev` `sha256:cfed7c51852e…` (uncommitted working tree) |
| Engine image | `openplayx/bittorrent-engine:5.1.0-r0` `sha256:a3402a55ef4e…`, qBittorrent v5.1.0, WebAPI 2.11.4 |
| App token / engine password | Mac: `build/deploy/secrets/bittorrent-211-{api-token,engine-password}` (gitignored, 0600) |

Results:

- [x] Isolated Linux e2e on 211 (`E2E_PREBUILT=1`, binaries cross-built on the Mac): 13/13 passed in 37 s; project removed afterwards, other containers untouched. Report: `~/openplayx-bittorrent-e2e/test/e2e/reports/last-run.{log,json}` on 211.
- [x] Deployed instance: `/readyz` ready, `/docs` 200, wrong token 401, capabilities show qBittorrent v5.1.0; WebUI not reachable from the LAN.
- [x] App tool chain against the deployed instance (`test/services/torrent_tools_test.dart` "real server", `TORRENT_E2E_NO_PEERS=1`): upload self-made `.torrent` → plan → add → metadata/stalled state → pause → resume → limits → delete-files denied by access scope → private tracker magnet rejected (`network_address_not_allowed`) → remove keeping files.
- [ ] Full download through the deployed instance: not run there, because production policy (correctly) rejects private-network trackers and no public test content is used; full downloads are covered by the e2e run on the same host and images.
- [ ] App UI walkthrough on a device, host reboot, host-offline behaviour: pending.

## Earlier local verification (Docker Desktop, arm64)

All thirteen e2e cases passed against qbittorrent-nox v5.1.0 (WebAPI 2.11.4):
health/auth/docs, `.torrent` add with SHA-256 integrity, idempotent create,
v1/v2/hybrid multi-file, file selection, magnet metadata discovery, peerless
magnet cancellation, content Range/HEAD, pause/resume/limits, restart
recovery, engine offline (stale), remove keeping files, delete only task files.
The same suite then passed on 211 (above).
