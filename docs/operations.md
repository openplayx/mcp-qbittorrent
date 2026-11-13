# Operations

## One-time setup on a deploy host

1. Pick an isolated directory and project name, e.g. `/srv/openplayx-bittorrent`
   and `openplayx-bittorrent`. Check existing containers/ports first
   (`docker ps`, `ss -ltnp`); never reuse another stack's directory or ports.
2. Create the download root (dedicated, not a shared production download dir)
   and make it writable by `PUID:PGID`.
3. Generate credentials (on any machine with the binary):
   ```bash
   bittorrent-server new-token openplayx-app      # token for the App + secrets entry
   printf '%s\n' "$ENGINE_PASSWORD" | bittorrent-server engine-password-hash
   ```
4. On the host, in `<dir>/deploy/`:
   - `.env` from `env.example` (mode 0600): project name, ports, `DOWNLOAD_ROOT`,
     `QBT_USERNAME`, `QBT_PASSWORD_PBKDF2`, `QBT_WEBUI_PORT`.
   - `config/config.json` from `config/config.example.json`. `engine.url` must be
     `http://qbittorrent:<QBT_WEBUI_PORT>`: the engine validates that the Host
     header port equals its listening port, so the port is identical inside and
     outside the container.
   - `config/secrets.json` from `config/credentials.example.json` (mode 0600):
     the token digest and the engine WebUI username/password.
5. From your workstation: `DOCKER="sudo docker" scripts/deploy.sh user@host /srv/openplayx-bittorrent`.

## Ports

| Port | Exposure | Purpose |
|---|---|---|
| `API_PORT` → 8780 | `API_BIND` (LAN/VPN IP, or 127.0.0.1 behind a TLS proxy) | authenticated API |
| `PEER_PORT` tcp+udp | public | BitTorrent peers |
| `QBT_WEBUI_PORT` | 127.0.0.1 only | native WebUI for maintainers via `ssh -L P:127.0.0.1:P` |

Serve the API over HTTPS for anything beyond a private LAN: set
`tls_cert_file`/`tls_key_file` in config.json or put a TLS reverse proxy in
front. The App refuses plain HTTP to public addresses.

## Engine security settings

`deploy/engine/entrypoint.sh` renders `qBittorrent.conf` on every start:
WebUI authentication, CSRF protection, clickjacking protection and Host header
validation stay on; external programs (AutoRun), UPnP and port forwarding are
off. The adapter sends a matching `Origin`/`Referer`; nothing is disabled for
convenience. Changing preferences in the WebUI does not survive a restart.

## Health, logs, rollback

```bash
scripts/healthcheck.sh user@host /srv/openplayx-bittorrent
ssh user@host 'cd /srv/openplayx-bittorrent/deploy && sudo docker compose logs --tail 100 api'
scripts/rollback.sh user@host /srv/openplayx-bittorrent   # previous API image tag
```

Request logs contain the route pattern, status and request ID only — never
query strings, bodies, magnets, tokens or paths.

## Behaviour to know

- Disk watermark: when free space drops below `limits.min_free_bytes`, running
  downloads are stopped (`hold_reason: disk_low`) and resume automatically
  once space is back above the mark plus 10%.
- Engine restarts or outages: tasks are kept; views show `stale: true`.
  Torrents removed directly in the WebUI become `error / engine_task_missing`
  and are never re-added automatically.
- Seeding uses upload bandwidth; `seeding.ratio_limit` and
  `seeding.seeding_time_minutes` bound it per torrent.
- Engine torrents not created by this service are never listed or modified.
- Cleanup: removing a task keeps its files by default; deleting files requires
  a remove plan with `delete_files: true` and removes only that task's own
  directory.
