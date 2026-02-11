#!/bin/bash
set -e

VERSION="v0.1.0-alpha"
BIN_DIR="$HOME/.local/bin"
HIDDEN_DAEMON="$BIN_DIR/.deskconnd"
SERVICE_NAME="deskconnd"
SERVICE_FILE="$HOME/.config/systemd/user/$SERVICE_NAME.service"

mkdir -p "$BIN_DIR"

echo "Downloading binaries..."
curl -L -o deskconnctl https://github.com/xconnio/deskconn/releases/download/$VERSION/deskconnctl
curl -L -o deskconnd https://github.com/xconnio/deskconn/releases/download/$VERSION/deskconnd

mv deskconnctl "$BIN_DIR/deskconnctl"
mv deskconnd "$HIDDEN_DAEMON"

chmod 755 "$BIN_DIR/deskconnctl"
chmod 700 "$HIDDEN_DAEMON"

echo "Binaries installed!"

echo "Setting up systemd user service for $SERVICE_NAME..."
mkdir -p "$(dirname "$SERVICE_FILE")"

cat > "$SERVICE_FILE" <<EOL
[Unit]
Description=DeskConn Daemon
After=network.target

[Service]
ExecStart=$HIDDEN_DAEMON
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOL

systemctl --user daemon-reload
systemctl --user enable $SERVICE_NAME
systemctl --user start $SERVICE_NAME

echo "Systemd service $SERVICE_NAME installed and started!"
