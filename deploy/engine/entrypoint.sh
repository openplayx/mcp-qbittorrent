#!/bin/sh
# Renders qBittorrent.conf from the environment on every start so the
# engine's security settings are deterministic: WebUI auth, CSRF and Host
# header validation stay on; external programs, UPnP and the WebUI's own
# HTTPS are off (the WebUI is only reachable on the private network).
# The engine's own login ban is off: behind the bittorrent-server WebUI proxy
# every browser shares one source address, so one guesser would lock out all
# users and the API adapter. The proxy limits failures per real client IP.
set -eu
: "${QBT_USERNAME:?QBT_USERNAME required}"
: "${QBT_PASSWORD_PBKDF2:?QBT_PASSWORD_PBKDF2 required}"
QBT_WEBUI_PORT="${QBT_WEBUI_PORT:-8080}"
QBT_PEER_PORT="${QBT_PEER_PORT:-6881}"
QBT_SERVER_DOMAINS="${QBT_SERVER_DOMAINS:-qbittorrent;localhost;127.0.0.1}"
QBT_DHT="${QBT_DHT:-true}"
QBT_PEX="${QBT_PEX:-true}"
QBT_LSD="${QBT_LSD:-false}"
QBT_MAX_ACTIVE_DOWNLOADS="${QBT_MAX_ACTIVE_DOWNLOADS:-5}"
QBT_MAX_ACTIVE_TORRENTS="${QBT_MAX_ACTIVE_TORRENTS:-20}"
dir=/config/qBittorrent/config
mkdir -p "$dir" /config/qBittorrent/data
cat > "$dir/qBittorrent.conf" <<CONF
[LegalNotice]
Accepted=true

[AutoRun]
enabled=false
OnTorrentAdded\\Enabled=false

[BitTorrent]
Session\\DefaultSavePath=/downloads/.unassigned
Session\\TempPathEnabled=false
Session\\Port=${QBT_PEER_PORT}
Session\\DHTEnabled=${QBT_DHT}
Session\\PeXEnabled=${QBT_PEX}
Session\\LSDEnabled=${QBT_LSD}
Session\\QueueingSystemEnabled=true
Session\\MaxActiveDownloads=${QBT_MAX_ACTIVE_DOWNLOADS}
Session\\MaxActiveTorrents=${QBT_MAX_ACTIVE_TORRENTS}
Session\\MaxActiveUploads=${QBT_MAX_ACTIVE_TORRENTS}
Session\\Preallocation=false
Session\\AddExtensionToIncompleteFiles=false

[Network]
PortForwardingEnabled=false

[Preferences]
Connection\\UPnP=false
General\\Locale=en
WebUI\\Enabled=true
WebUI\\Address=*
WebUI\\Port=${QBT_WEBUI_PORT}
WebUI\\Username=${QBT_USERNAME}
WebUI\\Password_PBKDF2="${QBT_PASSWORD_PBKDF2}"
WebUI\\LocalHostAuth=true
WebUI\\AuthSubnetWhitelistEnabled=false
WebUI\\CSRFProtection=true
WebUI\\ClickjackingProtection=true
WebUI\\HostHeaderValidation=true
WebUI\\MaxAuthenticationFailCount=0
WebUI\\ServerDomains="${QBT_SERVER_DOMAINS}"
WebUI\\UseUPnP=false
WebUI\\HTTPS\\Enabled=false
CONF
exec qbittorrent-nox --profile=/config --confirm-legal-notice
