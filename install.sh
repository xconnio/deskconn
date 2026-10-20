#!/bin/bash
set -e

REPO="xconnio/deskconn"
BIN_DIR="$HOME/.local/bin"
EXEC_DIR="$HOME/.local/lib/exec"

case "$(uname -s)" in
    Linux)
        OS="linux"
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

if [ ! -f "$TMP_DIR/deskconn" ]; then
    echo "Release archive does not contain the deskconn binary."
    exit 1
fi

rm -f "$BIN_DIR/desk" "$BIN_DIR/deskconn" "$EXEC_DIR/deskconn-userd" "$EXEC_DIR/deskconn-rootd"

mv "$TMP_DIR/deskconn" "$EXEC_DIR/deskconn"
chmod 755 "$EXEC_DIR/deskconn"
ln -sf "$EXEC_DIR/deskconn" "$BIN_DIR/desk"

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

    # systemd --user services don't reliably inherit DISPLAY/WAYLAND_DISPLAY from
    # the desktop session, so deskconn-userd can't tell a desktop from a headless
    # server apart without them being exported explicitly. Capture them from the
    # installer's own environment: on a real desktop session they'll be set here;
    # on a headless server they won't, and deskconn-userd will register server-only
    # APIs (no wallpaper/display RPCs).
    local userd_env_lines="Environment=TERM=xterm-256color"
    if [ -n "${DISPLAY:-}" ]; then
        userd_env_lines="$userd_env_lines
Environment=DISPLAY=$DISPLAY"
    fi
    if [ -n "${WAYLAND_DISPLAY:-}" ]; then
        userd_env_lines="$userd_env_lines
Environment=WAYLAND_DISPLAY=$WAYLAND_DISPLAY"
    fi

    echo "Setting up systemd user services..."
    mkdir -p "$systemd_user_dir"

    # deskconn-userd runs its connectivity (cloud, LAN, remote-client auth, QUIC/WebRTC streams)
    # in-process, through xconn. Earlier installs ran it as a separate xlink service, and then
    # the daemon as deskconnd: remove those, or they would compete with deskconn-userd for
    # deskconn.sock.
    for old_service in xlink deskconnd; do
        if [ -f "$systemd_user_dir/$old_service.service" ]; then
            echo "Removing the old $old_service service (now deskconn-userd)..."
            systemctl --user stop "$old_service" 2>/dev/null || true
            systemctl --user disable "$old_service" 2>/dev/null || true
            rm -f "$systemd_user_dir/$old_service.service"
        fi
        rm -f "$EXEC_DIR/$old_service"
    done
    rm -f "$BIN_DIR/vpnd"

    # Flags added with "systemctl --user edit deskconn-userd" override ExecStart in a drop-in,
    # which still names the daemon's old path on older installs, and its old --standalone flag
    # (now "--mode standalone").
    for drop_in in "$systemd_user_dir/deskconn-userd.service.d"/*.conf; do
        [ -f "$drop_in" ] || continue
        sed -i -E \
            -e "s|^ExecStart=$EXEC_DIR/deskconn-userd( \|\$)|ExecStart=$EXEC_DIR/deskconn serve --mode user\1|" \
            -e '/^ExecStart=.* serve --mode user( .*)? --standalone( |$)/{s/ --standalone( |$)/\1/;s/ serve --mode user/ serve --mode standalone/}' \
            "$drop_in"
    done

    cat > "$systemd_user_dir/deskconn-userd.service" <<EOL
[Unit]
Description=deskconn user daemon (connectivity, shell, files, VPN control, ...)
After=network.target

[Service]
ExecStart=$EXEC_DIR/deskconn serve --mode user
Restart=always
RestartSec=5
$userd_env_lines

[Install]
WantedBy=default.target
EOL

    systemctl --user daemon-reload

    if systemctl --user is-enabled --quiet deskconn-userd; then
        echo "Service deskconn-userd exists. Restarting..."
        systemctl --user restart deskconn-userd
    else
        echo "Enabling and starting deskconn-userd..."
        systemctl --user enable deskconn-userd
        systemctl --user start deskconn-userd
    fi
    echo "Systemd service deskconn-userd installed and started!"
}

install_service_linux
