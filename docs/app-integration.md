# OpenPlayX App integration

The App connects in **Settings → BitTorrent 服务** (name, HTTPS address, API
token, default destination, refresh interval, whether file deletion is
allowed). The token is stored in the platform secure storage only.

## AI tools

`lib/services/ai_tools/torrent_tools.dart` registers twelve tools. They are
offered to the model only after a server is configured.

| Tool | Server call | Notes |
|---|---|---|
| `torrent_service_info` | `GET /v1/capabilities` | servers, destinations, limits, user-uploaded `input_ref`s |
| `torrent_plan` | `POST /v1/torrent-plans` | magnet must appear in the user's messages; returns host `plan_ref` |
| `torrent_add` | `POST /v1/torrents` | idempotency key derived from the plan |
| `torrent_list` / `torrent_get` | `GET /v1/torrents[/{id}]` | list cached for the refresh interval |
| `torrent_files` | `GET /v1/torrents/{id}/files` | issues `file_ref`s |
| `torrent_select_files` | `PATCH …/selection` | preset or one `file_ref` + only/add/remove; user confirms |
| `torrent_pause` / `torrent_resume` | `POST …/pause|resume` | succeeds once the engine reports the state |
| `torrent_set_limits` | `PATCH …/limits` | KiB/s, within admin ceiling |
| `torrent_remove_preview` | `POST …/remove-plan` | `delete_files` only if allowed in settings |
| `torrent_remove` | `POST …/remove` | user confirms in the App; the model cannot self-confirm |

All references (`server_ref`, `task_ref`, `file_ref`, `plan_ref`,
`removal_ref`, `input_ref`) are host-issued. No tool takes a URL, token,
path or engine option. Results mark every task `location: server`,
`in_app_library: false`, `offline_on_device: false`.

## Uploading `.torrent` files

The settings page uploads a picked `.torrent` to the server and either starts
it directly or registers it for the assistant (`uploaded_inputs` in
`torrent_service_info`).

## Status in the UI

Chat tool cards and the settings page show "服务器下载 · <server>", state,
progress, speeds, seeding and errors, and say explicitly when a download is
complete only on the server. The settings page polls at the configured
interval while visible; closing the App does not cancel anything remotely.
