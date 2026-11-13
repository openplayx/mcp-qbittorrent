# qbittorrent-nox built from Alpine's package, pinned. This is a self-built
# image, not an official qBittorrent image. qBittorrent is GPL-2.0-or-later,
# libtorrent-rasterbar is BSD-3-Clause; the image ships the packages' own
# license files under /usr/share/licenses (see deploy/LICENSES.md).
FROM alpine:3.22
ARG QBT_PKG_VERSION=5.1.0-r0
RUN apk add --no-cache "qbittorrent-nox=${QBT_PKG_VERSION}" tini \
 && mkdir -p /usr/share/licenses/qbittorrent \
 && qbittorrent-nox --version > /usr/share/licenses/qbittorrent/VERSION
COPY entrypoint.sh /usr/local/bin/engine-entrypoint
RUN chmod 0755 /usr/local/bin/engine-entrypoint
ENV HOME=/config
VOLUME ["/config", "/downloads"]
EXPOSE 8080
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/engine-entrypoint"]
