#!/bin/bash
set -e

REPO="xconnio/deskconn"
BIN_DIR="$HOME/.local/bin"
EXEC_DIR="$HOME/.local/lib/exec"

case "$(uname -s)" in
    Linux)
        OS="linux"
        ;;
    Darwin)
        OS="darwin"
        ;;
    *)
        echo "Unsupported OS: $(uname -s). For Windows, use install.ps1 instead."
        exit 1
        ;;
esac

mkdir -p "$BIN_DIR"
mkdir -p "$EXEC_DIR"

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)
        GO_ARCH="amd64"
        ;;
    aarch64|arm64)
        GO_ARCH="arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

echo "Resolving latest release..."
VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')"
if [ -z "$VERSION" ]; then
    echo "Failed to determine latest release version."
    exit 1
fi

VERSION_NO_V="${VERSION#v}"
ARCHIVE="deskconn_${VERSION_NO_V}_${OS}_${GO_ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$VERSION/$ARCHIVE"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading $ARCHIVE from $DOWNLOAD_URL..."
curl -fL "$DOWNLOAD_URL" -o "$TMP_DIR/$ARCHIVE"

echo "Extracting archive..."
tar -xzf "$TMP_DIR/$ARCHIVE" -C "$TMP_DIR"

if [ ! -f "$TMP_DIR/desk" ] || [ ! -f "$TMP_DIR/deskconnd" ]; then
    echo "Release archive does not contain desk and deskconnd binaries."
    exit 1
fi

# Before the rename the CLI was installed as "deskconn" (with "desk" a symlink to it).
rm -f "$BIN_DIR/desk" "$BIN_DIR/deskconn"

mv "$TMP_DIR/desk" "$BIN_DIR/desk"
mv "$TMP_DIR/deskconnd" "$EXEC_DIR/deskconnd"

chmod 755 "$BIN_DIR/desk"
chmod 700 "$EXEC_DIR/deskconnd"

# desk runs as the privileged VPN helper when invoked by this name.
ln -sf "$BIN_DIR/desk" "$BIN_DIR/vpnd"

BASH_COMP_DIR="$HOME/.local/share/bash-completion/completions"
ZSH_COMP_DIR="$HOME/.local/share/zsh/site-functions"

mkdir -p "$BASH_COMP_DIR" "$ZSH_COMP_DIR"
rm -f "$BASH_COMP_DIR/deskconn" "$ZSH_COMP_DIR/_deskconn"

"$BIN_DIR/desk" --completion-script-bash > "$BASH_COMP_DIR/desk"
# ':' is in COMP_WORDBREAKS by default, which splits "device:path" at the colon.
sed -i '/_desk_bash_autocomplete() {/a\    COMP_WORDBREAKS=${COMP_WORDBREAKS//:}' "$BASH_COMP_DIR/desk"

# Make path completions behave like "cd": show only the last path segment
# in the menu, and don't add a trailing space after directories/"device:".
AWK_SCRIPT="$(mktemp)"
cat > "$AWK_SCRIPT" << 'AWKEOF'
{
    print
    if ($0 == "    COMPREPLY=( $(compgen -W \"${opts}\" -- ${cur}) )") {
        print "    compopt -o filenames 2>/dev/null || true"
        print "    if [ \"${#COMPREPLY[@]}\" -eq 1 ] && [[ \"${COMPREPLY[0]}\" == */ || \"${COMPREPLY[0]}\" == *: ]]; then"
        print "        compopt -o nospace 2>/dev/null || true"
        print "    fi"
    }
}
AWKEOF
awk -f "$AWK_SCRIPT" "$BASH_COMP_DIR/desk" > "$BASH_COMP_DIR/desk.tmp"
mv "$BASH_COMP_DIR/desk.tmp" "$BASH_COMP_DIR/desk"
rm -f "$AWK_SCRIPT"

"$BIN_DIR/desk" --completion-script-zsh > "$ZSH_COMP_DIR/_desk"

echo "Installed shell completions"
echo "Installed desk $VERSION"

# Add BIN_DIR to PATH in shell config files if not already present
add_to_path() {
    local file="$1"
    grep -qF '.local/bin' "$file" 2>/dev/null && return
    printf '\n# Added by deskconn installer\nexport PATH="$HOME/.local/bin:$PATH"\n' >> "$file"
    echo "  Updated $file"
}

case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *)
        echo "Adding $BIN_DIR to PATH..."
        [ -f "$HOME/.bashrc" ]        && add_to_path "$HOME/.bashrc"
        [ -f "$HOME/.bash_profile" ]  && add_to_path "$HOME/.bash_profile"
        [ -f "$HOME/.zshrc" ]         && add_to_path "$HOME/.zshrc"
        # Fall back to .profile if none of the above exist
        if [ ! -f "$HOME/.bashrc" ] && [ ! -f "$HOME/.bash_profile" ] && [ ! -f "$HOME/.zshrc" ]; then
            add_to_path "$HOME/.profile"
        fi
        echo "Run this to apply immediately:  export PATH=\"\$HOME/.local/bin:\$PATH\""
        ;;
esac

install_service_linux() {
    local systemd_user_dir="$HOME/.config/systemd/user"

    echo "Setting up systemd user services..."
    mkdir -p "$systemd_user_dir"

    # systemd --user services don't reliably inherit DISPLAY/WAYLAND_DISPLAY from
    # the desktop session, so deskconnd can't tell a desktop from a headless
    # server apart without them being exported explicitly. Capture them from the
    # installer's own environment: on a real desktop session they'll be set here;
    # on a headless server they won't, and deskconnd will register server-only
    # APIs (no screenshot/display RPCs).
    local deskconnd_env_lines="Environment=TERM=xterm-256color"
    if [ -n "${DISPLAY:-}" ]; then
        deskconnd_env_lines="$deskconnd_env_lines
Environment=DISPLAY=$DISPLAY"
    fi
    if [ -n "${WAYLAND_DISPLAY:-}" ]; then
        deskconnd_env_lines="$deskconnd_env_lines
Environment=WAYLAND_DISPLAY=$WAYLAND_DISPLAY"
    fi

    # deskconnd runs xlink (connectivity: cloud, LAN, remote-client auth, QUIC/WebRTC
    # streams) in-process. Earlier installs ran xlink as its own service: remove it,
    # or it would compete with deskconnd for deskconn.sock.
    if [ -f "$systemd_user_dir/xlink.service" ]; then
        echo "Removing the old xlink service (now part of deskconnd)..."
        systemctl --user stop xlink 2>/dev/null || true
        systemctl --user disable xlink 2>/dev/null || true
        rm -f "$systemd_user_dir/xlink.service"
    fi
    rm -f "$EXEC_DIR/xlink"

    cat > "$systemd_user_dir/deskconnd.service" <<EOL
[Unit]
Description=deskconnd daemon (connectivity, shell, files, screen, printer, VPN control, ...)
After=network.target

[Service]
ExecStart=$EXEC_DIR/deskconnd
Restart=always
RestartSec=5
$deskconnd_env_lines

[Install]
WantedBy=default.target
EOL

    systemctl --user daemon-reload

    if systemctl --user is-enabled --quiet deskconnd; then
        echo "Service deskconnd exists. Restarting..."
        systemctl --user restart deskconnd
    else
        echo "Enabling and starting deskconnd..."
        systemctl --user enable deskconnd
        systemctl --user start deskconnd
    fi
    echo "Systemd service deskconnd installed and started!"
}

install_service_darwin() {
    local label="com.deskconn.deskconnd"
    local plist_file="$HOME/Library/LaunchAgents/$label.plist"
    local log_dir="$HOME/Library/Logs/deskconn"

    echo "Setting up launchd agent for $SERVICE_NAME..."
    mkdir -p "$(dirname "$plist_file")" "$log_dir"

    cat > "$plist_file" <<EOL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$label</string>
    <key>ProgramArguments</key>
    <array>
        <string>$EXEC_DIR/deskconnd</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$log_dir/deskconnd.log</string>
    <key>StandardErrorPath</key>
    <string>$log_dir/deskconnd.err.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>TERM</key>
        <string>xterm-256color</string>
    </dict>
</dict>
</plist>
EOL

    if launchctl print "gui/$(id -u)/$label" > /dev/null 2>&1; then
        echo "Service exists. Restarting..."
        launchctl bootout "gui/$(id -u)/$label" > /dev/null 2>&1 || true
    fi

    echo "Enabling and starting service..."
    launchctl bootstrap "gui/$(id -u)" "$plist_file"
    launchctl enable "gui/$(id -u)/$label"
    echo "launchd agent $label installed and started!"
}

case "$OS" in
    linux)
        install_service_linux
        ;;
    darwin)
        install_service_darwin
        ;;
esac
