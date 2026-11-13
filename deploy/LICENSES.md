# Third-party components in the deployment

| Component | Source | License | Notes |
|---|---|---|---|
| qbittorrent-nox 5.1.0 | Alpine Linux package `qbittorrent-nox=5.1.0-r0` | GPL-2.0-or-later | Installed unmodified into `openplayx/bittorrent-engine`; source at https://github.com/qbittorrent/qBittorrent (tag `release-5.1.0`) and Alpine aports |
| libtorrent-rasterbar 2.0.11 | Alpine package (dependency) | BSD-3-Clause | |
| Qt 6 runtime | Alpine packages (dependencies) | LGPL-3.0 / GPL | |
| Alpine Linux 3.22 base | `alpine:3.22` | various (see `apk info -L`) | |
| tini | Alpine package | MIT | |

The engine image is **self-built** from Alpine packages; it is not an official
qBittorrent image. The API service talks to the engine only over its HTTP
WebUI API and does not link qBittorrent or libtorrent.

When distributing the engine image, ship or point to the corresponding
source for the GPL/LGPL components (the Alpine aports commit for 3.22 and the
upstream tags above). `docker run --rm openplayx/bittorrent-engine:5.1.0-r0
cat /usr/share/licenses/qbittorrent/VERSION` records the packaged version.
