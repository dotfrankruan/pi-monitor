#!/bin/sh
set -eu

REPOSITORY="dotfrankruan/pi-monitor"
SERVICE_NAME="pi-monitor"
SERVICE_USER="pi-monitor"
SERVICE_GROUP="pi-monitor"

VERSION="${PI_MONITOR_VERSION:-latest}"
CUSTOM_RELEASE_BASE="${PI_MONITOR_RELEASE_BASE:-}"
PORT="${PI_MONITOR_PORT:-49152}"
DATA_DIR="${PI_MONITOR_DATA_DIR:-/var/lib/pi-monitor}"
INSTALL_DIR="${PI_MONITOR_INSTALL_DIR:-/opt/pi-monitor}"

fail() {
    printf 'pi-monitor installer: %s\n' "$*" >&2
    exit 1
}

log() {
    printf '==> %s\n' "$*"
}

[ "$(id -u)" -eq 0 ] || fail "run this installer as root (for example, pipe it to sudo sh)"
[ "$(uname -s)" = "Linux" ] || fail "only Linux is supported"

case "$(uname -m)" in
    aarch64|arm64) ;;
    *) fail "this release supports ARM64 only; detected $(uname -m)" ;;
esac

case "$PORT" in
    ''|*[!0-9]*) fail "PI_MONITOR_PORT must be a number" ;;
esac
[ "$PORT" -ge 1024 ] && [ "$PORT" -le 65535 ] || fail "PI_MONITOR_PORT must be between 1024 and 65535"

case "$DATA_DIR:$INSTALL_DIR" in
    *' '*|*"\t"*) fail "install and data paths must not contain whitespace" ;;
esac
case "$DATA_DIR" in /*) ;; *) fail "PI_MONITOR_DATA_DIR must be an absolute path" ;; esac
case "$INSTALL_DIR" in /*) ;; *) fail "PI_MONITOR_INSTALL_DIR must be an absolute path" ;; esac

command -v systemctl >/dev/null 2>&1 || fail "systemd is required"
command -v sha256sum >/dev/null 2>&1 || fail "sha256sum is required"

if command -v curl >/dev/null 2>&1; then
    download() { curl --fail --silent --show-error --location --retry 3 --connect-timeout 15 --max-time 300 --output "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
    download() { wget --quiet --tries=3 --output-document="$2" "$1"; }
else
    fail "curl or wget is required"
fi

TEMP_DIR="$(mktemp -d /tmp/pi-monitor-install.XXXXXX)"
trap 'rm -rf "$TEMP_DIR"' EXIT HUP INT TERM

ASSET="pi-monitor-v0-linux-arm64"
if [ -n "$CUSTOM_RELEASE_BASE" ]; then
    RELEASE_BASE="${CUSTOM_RELEASE_BASE%/}"
elif [ "$VERSION" = "latest" ]; then
    RELEASE_BASE="https://github.com/$REPOSITORY/releases/latest/download"
else
    case "$VERSION" in v[0-9]*) ;; *) fail "PI_MONITOR_VERSION must be latest or a tag such as v0.1.0" ;; esac
    RELEASE_BASE="https://github.com/$REPOSITORY/releases/download/$VERSION"
fi

# Resolve the exact asset name from SHA256SUMS so the installer continues to
# work as the release version changes.
log "Downloading release checksums"
download "$RELEASE_BASE/SHA256SUMS" "$TEMP_DIR/SHA256SUMS"
ASSET="$(awk '$2 ~ /^pi-monitor-v[0-9].*-linux-arm64$/ { print $2; exit }' "$TEMP_DIR/SHA256SUMS")"
[ -n "$ASSET" ] || fail "SHA256SUMS does not contain a Linux ARM64 binary"

log "Downloading $ASSET"
download "$RELEASE_BASE/$ASSET" "$TEMP_DIR/$ASSET"

EXPECTED="$(awk -v asset="$ASSET" '$2 == asset { print $1; exit }' "$TEMP_DIR/SHA256SUMS")"
ACTUAL="$(sha256sum "$TEMP_DIR/$ASSET" | awk '{print $1}')"
[ -n "$EXPECTED" ] && [ "$ACTUAL" = "$EXPECTED" ] || fail "SHA-256 verification failed"
log "SHA-256 verified"

if ! getent group "$SERVICE_GROUP" >/dev/null 2>&1; then
    groupadd --system "$SERVICE_GROUP"
fi
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --gid "$SERVICE_GROUP" --home-dir "$DATA_DIR" --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

install -d -o root -g root -m 0755 "$INSTALL_DIR"
install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0750 "$DATA_DIR"

if systemctl is-active --quiet "$SERVICE_NAME.service"; then
    log "Stopping existing service"
    systemctl stop "$SERVICE_NAME.service"
fi

install -o root -g root -m 0755 "$TEMP_DIR/$ASSET" "$INSTALL_DIR/pi-monitor"
chown -R "$SERVICE_USER:$SERVICE_GROUP" "$DATA_DIR"

cat >"/etc/systemd/system/$SERVICE_NAME.service" <<EOF
[Unit]
Description=Raspberry Pi system monitor
After=network.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_GROUP
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/pi-monitor -listen 0.0.0.0:$PORT -data-dir $DATA_DIR -sample-interval 500ms -persist-interval 5s -flush-interval 1h
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now "$SERVICE_NAME.service"

log "Installed $($INSTALL_DIR/pi-monitor -version)"
log "Service is active: $(systemctl is-active "$SERVICE_NAME.service")"
log "Open http://$(hostname -I 2>/dev/null | awk '{print $1}'):$PORT"
