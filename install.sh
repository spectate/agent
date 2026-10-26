#!/bin/sh
# Spectate host agent installer (Linux with systemd, macOS).
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
#   --auto-update       also install a daily timer that runs `spectate-agent update --if-newer` (Linux)
#
# Linux: creates the unprivileged `spectate-agent` user, installs the binary to
# /usr/local/bin, writes /etc/spectate-agent/agent.env, registers the host and
# enables the hardened systemd unit. State lives in /var/lib/spectate-agent.
#
# macOS: installs the binary to /usr/local/bin (the only step that needs sudo),
# then registers the host and installs a launchd agent as the user who ran sudo.
# State lives in ~/Library/Application Support/spectate-agent, the log in
# ~/Library/Logs/spectate-agent.log.
#
# Downloads are verified against the release's checksums.txt. The release
# assets' copy of this script also verifies the ed25519 signature of
# checksums.txt (checksums.txt.sig) when OpenSSL 3 is available;
# `spectate-agent update` always verifies it.
set -eu

REPO="spectate/agent"
# Set to the release version in the install.sh asset of each GitHub release;
# empty in the repository, which installs the newest v2 release.
DEFAULT_VERSION=""
# Release signing public key (base64 ed25519), set in the install.sh asset of
# each GitHub release; empty in the repository (checksums only).
UPDATE_PUBLIC_KEY=""
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
AUTO_UPDATE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --api-url) API_URL="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --binary) LOCAL_BINARY="$2"; shift 2 ;;
    --import-legacy) IMPORT_LEGACY=1; shift ;;
    --upgrade) UPGRADE=1; shift ;;
    --auto-update) AUTO_UPDATE=1; shift ;;
    -h|--help) sed -n '2,28p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

fail() { echo "error: $*" >&2; exit 1; }

# ed25519 verification needs OpenSSL 3 (`pkeyutl -rawin`); LibreSSL and
# OpenSSL 1.1 cannot do it.
have_openssl3() {
  command -v openssl >/dev/null 2>&1 || return 1
  case "$(openssl version 2>/dev/null)" in
    "OpenSSL 3."* | "OpenSSL 4."*) return 0 ;;
  esac
  return 1
}

# verify_signature DIR BASE_URL: checks DIR/checksums.txt against the
# release's checksums.txt.sig with UPDATE_PUBLIC_KEY.
verify_signature() {
  [ -n "$UPDATE_PUBLIC_KEY" ] || return 0
  if ! have_openssl3; then
    echo "warning: OpenSSL 3 not found: verifying checksums only, not the release signature" >&2
    return 0
  fi
  curl -fsSL "$2/checksums.txt.sig" -o "$1/checksums.txt.sig" \
    || fail "the release has no checksums.txt.sig (unsigned release?)"
  # SubjectPublicKeyInfo of a raw ed25519 key: a fixed 12-byte DER prefix.
  printf '%s\n' '-----BEGIN PUBLIC KEY-----' "MCowBQYDK2VwAyEA$UPDATE_PUBLIC_KEY" '-----END PUBLIC KEY-----' > "$1/release.pem"
  tr -d ' \r\n' < "$1/checksums.txt.sig" | openssl base64 -d -A > "$1/checksums.sig.bin" \
    || fail "malformed checksums.txt.sig"
  openssl pkeyutl -verify -pubin -inkey "$1/release.pem" -rawin \
    -in "$1/checksums.txt" -sigfile "$1/checksums.sig.bin" >/dev/null 2>&1 \
    || fail "signature verification of checksums.txt failed"
  echo "Verified the release signature."
}

[ "$(id -u)" -eq 0 ] || fail "please run as root (sudo)"
case "$(uname -s)" in
  Linux)
    OS=linux
    command -v systemctl >/dev/null 2>&1 || fail "systemd is required"
    ;;
  Darwin)
    OS=darwin
    [ "$AUTO_UPDATE" -eq 0 ] || fail "--auto-update needs systemd; on macOS update with: sudo spectate-agent update"
    # The agent runs as a launchd agent of the user who ran sudo, not as root
    RUN_USER="${SUDO_USER:-}"
    [ -n "$RUN_USER" ] && [ "$RUN_USER" != "root" ] \
      || fail "on macOS, run the installer with sudo from the account the agent should run as"
    RUN_UID="$(id -u "$RUN_USER")"
    RUN_HOME="$(dscl . -read "/Users/$RUN_USER" NFSHomeDirectory 2>/dev/null | sed 's/^NFSHomeDirectory: *//')"
    [ -n "$RUN_HOME" ] || RUN_HOME="$(eval echo "~$RUN_USER")"
    ;;
  *) fail "unsupported system $(uname -s): the installer supports Linux (systemd) and macOS" ;;
esac

# sha256sum on Linux, shasum on macOS
sha256_check() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c -
  else
    shasum -a 256 -c -
  fi
}

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
    # The newest stable release (pre-releases are never "latest")
    BASE="https://github.com/$REPO/releases/latest/download"
    echo "Downloading the latest spectate-agent ($OS/$ARCH)..."
  else
    VERSION="${VERSION#v}"
    BASE="https://github.com/$REPO/releases/download/v$VERSION"
    echo "Downloading spectate-agent $VERSION ($OS/$ARCH)..."
  fi
  ASSET="spectate-agent_${OS}_$ARCH"
  curl -fsSL "$BASE/$ASSET" -o "$TMP/spectate-agent"
  curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"
  verify_signature "$TMP" "$BASE"
  (cd "$TMP" && grep " $ASSET\$" checksums.txt | sed "s/$ASSET/spectate-agent/" | sha256_check) \
    || fail "checksum verification failed"
fi
mkdir -p "$(dirname "$BIN")"
install -m 0755 "$TMP/spectate-agent" "$BIN"
echo "Installed $("$BIN" version)"

if [ "$OS" = "darwin" ]; then
  # Runs a command as RUN_USER in that user's launchd session (so `launchctl
  # load` reaches the user's agents) with the user's HOME.
  as_user() {
    launchctl asuser "$RUN_UID" sudo -H -u "$RUN_USER" "$@"
  }
  PLIST="$RUN_HOME/Library/LaunchAgents/net.spectate.agent.plist"

  if [ "$UPGRADE" -eq 1 ]; then
    launchctl kickstart -k "gui/$RUN_UID/net.spectate.agent" 2>/dev/null \
      || fail "upgraded the binary, but could not restart the agent; run: launchctl kickstart -k gui/$RUN_UID/net.spectate.agent"
    echo "Upgraded and restarted spectate-agent."
    exit 0
  fi

  if [ -f "$RUN_HOME/Library/Application Support/spectate-agent/state.json" ]; then
    echo "Already registered; keeping the existing identity."
  elif [ "$IMPORT_LEGACY" -eq 1 ]; then
    as_user "$BIN" import-legacy --api-url "$API_URL"
    if command -v spectated >/dev/null 2>&1; then
      echo "Removing the v1 agent (spectated)..."
      as_user spectated uninstall || true
    fi
  else
    # The token goes through the environment, not the command line (ps)
    SPECTATE_INSTALL_TOKEN="$TOKEN" launchctl asuser "$RUN_UID" \
      sudo --preserve-env=SPECTATE_INSTALL_TOKEN -H -u "$RUN_USER" "$BIN" register --api-url "$API_URL"
  fi

  # Reinstalling replaces the launchd agent: unload the old one first
  [ -f "$PLIST" ] && as_user launchctl unload "$PLIST" 2>/dev/null || true
  as_user "$BIN" install
  echo
  echo "Done. Logs: tail -f ~/Library/Logs/spectate-agent.log   Status: spectate-agent status"
  exit 0
fi

if [ "$UPGRADE" -eq 1 ]; then
  if [ "$AUTO_UPDATE" -eq 1 ]; then
    "$BIN" install --user "$USER_NAME" --env-file "$ETC_DIR/agent.env" --auto-update
  fi
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

# 5. Service (and the optional daily update timer)
if [ "$AUTO_UPDATE" -eq 1 ]; then
  "$BIN" install --user "$USER_NAME" --env-file "$ETC_DIR/agent.env" --auto-update
else
  "$BIN" install --user "$USER_NAME" --env-file "$ETC_DIR/agent.env"
fi
echo
echo "Done. Logs: journalctl -u spectate-agent -f   Status: sudo -u $USER_NAME SPECTATE_STATE_DIR=$STATE_DIR $BIN status"
