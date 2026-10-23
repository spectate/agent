#!/bin/sh
# Spectate host agent installer (Linux, systemd).
#
#   curl -fsSL https://get.spectate.net/agent.sh | sudo sh -s -- --token <install token>
#
# Options:
#   --token TOKEN       team install token (or SPECTATE_INSTALL_TOKEN)
#   --api-url URL       API base URL (default https://api.spectate.net)
#   --version VERSION   release to install, e.g. 2.0.1 (default: this
#                       installer's release, else the newest v2 release)
#   --binary PATH       install a local binary instead of downloading
#   --import-legacy     reuse the v1 agent's (spectated) host token instead of registering
#   --upgrade           only replace the binary and restart the service
#
# Creates the unprivileged `spectate-agent` user, installs the binary to
# /usr/local/bin, writes /etc/spectate-agent/agent.env, registers the host and
# enables the hardened systemd unit. State lives in /var/lib/spectate-agent.
set -eu

REPO="spectate/agent"
# Set to the release version in the install.sh asset of each GitHub release;
# empty in the repository, which installs the newest v2 release.
DEFAULT_VERSION=""
BIN="/usr/local/bin/spectate-agent"
USER_NAME="spectate-agent"
ETC_DIR="/etc/spectate-agent"
STATE_DIR="/var/lib/spectate-agent"

TOKEN="${SPECTATE_INSTALL_TOKEN:-}"
API_URL="${SPECTATE_API_URL:-https://api.spectate.net}"
VERSION="${DEFAULT_VERSION:-latest}"
LOCAL_BINARY=""
IMPORT_LEGACY=0
UPGRADE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --api-url) API_URL="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --binary) LOCAL_BINARY="$2"; shift 2 ;;
    --import-legacy) IMPORT_LEGACY=1; shift ;;
    --upgrade) UPGRADE=1; shift ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

fail() { echo "error: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || fail "please run as root (sudo)"
[ "$(uname -s)" = "Linux" ] || fail "this installer supports Linux; on macOS run \`spectate-agent install\` as your user"
command -v systemctl >/dev/null 2>&1 || fail "systemd is required"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac

if [ "$UPGRADE" -eq 0 ] && [ "$IMPORT_LEGACY" -eq 0 ] && [ -z "$TOKEN" ]; then
  fail "--token is required (or --import-legacy / --upgrade)"
fi

# 1. Binary
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
if [ -n "$LOCAL_BINARY" ]; then
  cp "$LOCAL_BINARY" "$TMP/spectate-agent"
else
  if [ "$VERSION" = "latest" ]; then
    # Not releases/latest: that stays on the v1 agent (spectated) while v1
    # installs are supported. Releases are listed newest first; pre-releases
    # (v2.1.0-rc.1) do not match.
    VERSION="$(curl -fsSL -H 'Accept: application/vnd.github+json' \
      "https://api.github.com/repos/$REPO/releases?per_page=30" \
      | grep -o '"tag_name": *"v2\.[0-9][0-9]*\.[0-9][0-9]*"' | head -n 1 | sed 's/.*"v\(.*\)"/\1/')" \
      || VERSION=""
    [ -n "$VERSION" ] || fail "could not find the latest release; pass --version (see https://github.com/$REPO/releases)"
  fi
  VERSION="${VERSION#v}"
  BASE="https://github.com/$REPO/releases/download/v$VERSION"
  echo "Downloading spectate-agent $VERSION (linux/$ARCH)..."
  curl -fsSL "$BASE/spectate-agent_linux_$ARCH" -o "$TMP/spectate-agent"
  curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"
  (cd "$TMP" && grep " spectate-agent_linux_$ARCH\$" checksums.txt | sed "s/spectate-agent_linux_$ARCH/spectate-agent/" | sha256sum -c -) \
    || fail "checksum verification failed"
fi
install -m 0755 "$TMP/spectate-agent" "$BIN"
echo "Installed $("$BIN" version)"

if [ "$UPGRADE" -eq 1 ]; then
  systemctl restart spectate-agent
  echo "Upgraded and restarted spectate-agent."
  exit 0
fi

# 2. Unprivileged service user
if ! id "$USER_NAME" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir "$STATE_DIR" --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null \
    || useradd -r -M -d "$STATE_DIR" -s /sbin/nologin "$USER_NAME"
fi

# 3. Config + state directories
install -d -m 0755 "$ETC_DIR"
if [ ! -f "$ETC_DIR/agent.env" ]; then
  cat > "$ETC_DIR/agent.env" <<EOF
# spectate-agent settings (see \`spectate-agent help\`)
SPECTATE_API_URL=$API_URL
#SPECTATE_LOG_LEVEL=info
#SPECTATE_SPOOL_MAX_MB=50
#SPECTATE_SPOOL_MAX_AGE=24h
EOF
fi
install -d -m 0700 -o "$USER_NAME" -g "$USER_NAME" "$STATE_DIR"

# 4. Identity: register, or take over the v1 agent's host token.
as_agent() {
  if command -v runuser >/dev/null 2>&1; then
    runuser -u "$USER_NAME" -- env SPECTATE_STATE_DIR="$STATE_DIR" SPECTATE_API_URL="$API_URL" "$@"
  else
    su -s /bin/sh "$USER_NAME" -c "SPECTATE_STATE_DIR='$STATE_DIR' SPECTATE_API_URL='$API_URL' $*"
  fi
}

if [ -f "$STATE_DIR/state.json" ]; then
  echo "Already registered; keeping the existing identity."
elif [ "$IMPORT_LEGACY" -eq 1 ]; then
  SPECTATE_STATE_DIR="$STATE_DIR" "$BIN" import-legacy --api-url "$API_URL"
  chown "$USER_NAME:$USER_NAME" "$STATE_DIR/state.json"
  if command -v spectated >/dev/null 2>&1; then
    echo "Removing the v1 agent service (spectated)..."
    spectated uninstall || true
  fi
else
  SPECTATE_INSTALL_TOKEN="$TOKEN" as_agent "$BIN" register --api-url "$API_URL"
fi

# 5. Service
"$BIN" install --user "$USER_NAME" --env-file "$ETC_DIR/agent.env"
echo
echo "Done. Logs: journalctl -u spectate-agent -f   Status: sudo -u $USER_NAME SPECTATE_STATE_DIR=$STATE_DIR $BIN status"
