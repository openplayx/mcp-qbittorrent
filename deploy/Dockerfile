# API service image. The binary is built by scripts/build.sh (static, no cgo)
# so the deploy host needs no Go toolchain or module downloads.
FROM alpine:3.22
ARG TARGETARCH
RUN apk add --no-cache ca-certificates tzdata tini
COPY dist/bittorrent-server-linux-${TARGETARCH} /usr/local/bin/bittorrent-server
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/bittorrent-server"]
CMD ["-config", "/etc/bittorrent-server/config.json"]
EXPOSE 8780
