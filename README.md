# OpenPlayX BitTorrent Server

A restricted HTTP API over a pinned **qbittorrent-nox 5.1.0** engine. The
OpenPlayX App calls it through host-registered AI tools to add magnet /
`.torrent` downloads, follow progress, select files, pause, resume, limit and
remove tasks. Downloads run on the server and keep going when the App closes.

Design: `design/tech/bittorrent_server.md` in the OpenPlayX design repository.

## What it is (and is not)

- Accepts only user-provided `magnet:` links and uploaded `.torrent` files.
  No search sites, indexers, remote torrent URLs (`xs`/`as`) or local paths.
- Every nested address (trackers, web seeds, peer hints) is checked against
  the network policy, not just the outer scheme.
- Callers use logical `destination_ref`s and service task IDs. Engine
  credentials, SIDs, full magnets and absolute paths are never returned.
- Each task downloads into its own directory below the configured root;
  removal keeps files unless a separately previewed plan says otherwise.
- The engine is the authority for download state; the service reports
  `stale` / `engine_status: unavailable` instead of pretending.

## Layout

```text
cmd/server/          API entry; also `new-token` and `engine-password-hash`
internal/api/        auth, rate limit, request/response, errors
internal/torrents/   inputs, plans, ownership, operations, reconcile, state
internal/qbittorrent qBittorrent 5.x WebUI API adapter (version-pinned)
internal/storage/    SQLite: tasks, plans, idempotent operations, audit
internal/files/      authorised file references, symlink-safe reads/deletes
api/                 OpenAPI 3.1 (served at /openapi.json) and /docs page
deploy/              Compose, API and engine images, config templates
scripts/             build, deploy, healthcheck, rollback, e2e
test/fakeqb/         in-process fake engine used by API tests
test/e2e/            Docker end-to-end suite against real qbittorrent-nox
docs/                operations, App integration, 21177 acceptance
```

## Develop and test

```bash
go test -race ./...          # unit + API tests against the fake engine
scripts/e2e.sh               # real qbittorrent-nox in Docker (isolated network)
scripts/build.sh linux/amd64 # static binary in dist/
```

`scripts/e2e.sh` builds both images, generates throwaway credentials, starts
an isolated stack (the engine under test has no internet route; DHT/PeX/LSD
off), creates v1/v2/hybrid fixtures with a second engine acting as seeder and
tracker, and checks every acceptance item from the design (§10.2). It only
removes its own compose project and work directory.

## API at a glance

| Method & path | Purpose |
|---|---|
| `GET /healthz`, `GET /readyz` | liveness; database + engine readiness |
| `GET /v1/capabilities`, `GET /v1/destinations` | engine version, limits, destinations |
| `POST /v1/torrent-inputs` | upload a `.torrent` → short-lived `input_ref` |
| `POST /v1/torrent-plans` | validate magnet/input + destination → expiring plan |
| `POST /v1/torrents` | create from plan (idempotency key required) |
| `GET /v1/torrents[/{id}]`, `GET /v1/torrents/{id}/files` | state, progress, files |
| `PATCH /v1/torrents/{id}/selection` | change files against `files_revision` |
| `POST /v1/torrents/{id}/pause` / `resume` | confirmed against engine state |
| `PATCH /v1/torrents/{id}/limits` | per-task rates within admin ceilings |
| `POST /v1/torrents/{id}/remove-plan` / `remove` | preview, then apply |
| `GET /v1/operations/{id}` | async operation result |
| `GET\|HEAD /v1/torrents/{id}/files/{file_id}/content` | complete files, Range supported |

Full schema: `api/openapi.json`, rendered at `/docs`.

## License

GNU GPL v3, see `COPYING`. Engine licensing: `deploy/LICENSES.md`.
