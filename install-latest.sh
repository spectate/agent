#!/bin/bash

set -e

TOKEN=$1
OS=$2
ARCH=$3

if [ -z "$TOKEN" ]; then
  echo "Missing token"
  exit 1
fi

if [ -z "$OS" ]; then
  echo "Missing OS"
  exit 1
fi

if [ -z "$ARCH" ]; then
  echo "Missing ARCH"
  exit 1
fi

if [[ "$OS" != "linux" && "$OS" != "darwin" ]]; then
  echo "Unsupported operating system: $OS"
  exit 1
fi

if [[ "$ARCH" != "amd64" && "$ARCH" != "arm64" ]]; then
  echo "Unsupported architecture: $ARCH"
  exit 1
fi

# Check if current user has privileges for installing
if [ "$EUID" -ne 0 ]; then
  echo "Please run as root"
  exit 1
fi

ARTIFACT_URL="https://github.com/spectate/agent/releases/latest/download/spectated_${OS}_${ARCH}.tar.gz"

# Download and install
echo -n "Downloading latest release $ARTIFACT_URL... "
tar -xzf <(curl -Ls "$ARTIFACT_URL") -C /usr/bin
echo "done"

# Make it executable
echo -n "Making spectated executable... "
chmod +x /usr/bin/spectated
echo "done"

# Install service
echo -n "Installing spectated as service... "
/usr/bin/spectated install
echo "done"

# Run auth command
echo -n "Registering this host in Spectate... "
/usr/bin/spectated auth "$TOKEN"
echo "done"

# Start service
echo -n "Starting spectated service... "
if [[ "$OS" == "linux" ]]; then
  if [[ -x "$(command -v systemctl)" ]]; then
    systemctl start spectated
  else
    service spectated start
  fi
elif [[ "$OS" == "darwin" ]]; then
  launchctl load -w ~/Library/LaunchAgents/spectated.plist
fi
echo "done"
