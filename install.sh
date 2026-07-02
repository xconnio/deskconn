#!/bin/bash
set -e

REPO="xconnio/deskconn"
BIN_DIR="$HOME/.local/bin"
EXEC_DIR="$HOME/.local/lib/exec"
SERVICE_NAME="deskconnd"
SERVICE_FILE="$HOME/.config/systemd/user/$SERVICE_NAME.service"

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
ARCHIVE="deskconn_${VERSION_NO_V}_linux_${GO_ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$VERSION/$ARCHIVE"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading $ARCHIVE from $DOWNLOAD_URL..."
curl -fL "$DOWNLOAD_URL" -o "$TMP_DIR/$ARCHIVE"

echo "Extracting archive..."
tar -xzf "$TMP_DIR/$ARCHIVE" -C "$TMP_DIR"

if [ ! -f "$TMP_DIR/deskconn" ] || [ ! -f "$TMP_DIR/deskconnd" ]; then
    echo "Release archive does not contain deskconn and deskconnd binaries."
    exit 1
fi

mv "$TMP_DIR/deskconn" "$BIN_DIR/deskconn"
mv "$TMP_DIR/deskconnd" "$EXEC_DIR/deskconnd"

chmod 755 "$BIN_DIR/deskconn"
chmod 700 "$EXEC_DIR/deskconnd"

ln -sf "$BIN_DIR/deskconn" "$BIN_DIR/desk"

BASH_COMP_DIR="$HOME/.local/share/bash-completion/completions"
ZSH_COMP_DIR="$HOME/.local/share/zsh/site-functions"

mkdir -p "$BASH_COMP_DIR" "$ZSH_COMP_DIR"

"$BIN_DIR/deskconn" --completion-script-bash > "$BASH_COMP_DIR/deskconn"
# ':' is in COMP_WORDBREAKS by default, which splits "device:path" at the colon.
sed -i '/_deskconn_bash_autocomplete() {/a\    COMP_WORDBREAKS=${COMP_WORDBREAKS//:}' "$BASH_COMP_DIR/deskconn"

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
awk -f "$AWK_SCRIPT" "$BASH_COMP_DIR/deskconn" > "$BASH_COMP_DIR/deskconn.tmp"
mv "$BASH_COMP_DIR/deskconn.tmp" "$BASH_COMP_DIR/deskconn"
rm -f "$AWK_SCRIPT"

sed 's/complete -F _deskconn_bash_autocomplete -o default deskconn/complete -F _deskconn_bash_autocomplete -o default desk/' \
    "$BASH_COMP_DIR/deskconn" > "$BASH_COMP_DIR/desk"

"$BIN_DIR/deskconn" --completion-script-zsh > "$ZSH_COMP_DIR/_deskconn"
printf '#compdef desk\n_deskconn "$@"\n' > "$ZSH_COMP_DIR/_desk"

echo "Installed shell completions"
echo "Installed deskconn $VERSION"

# Add BIN_DIR to PATH in shell config files if not already present
add_to_path() {
    local file="$1"
    grep -qF '.local/bin' "$file" 2>/dev/null && return
    printf '\n# Added by deskconn installer\nexport PATH="$HOME/.local/bin:$PATH"\n' >> "$file"
    echo "  Updated $file"
}

if [[ ":$PATH:" != *":$BIN_DIR:"* ]]; then
    echo "Adding $BIN_DIR to PATH..."
    [ -f "$HOME/.bashrc" ]        && add_to_path "$HOME/.bashrc"
    [ -f "$HOME/.bash_profile" ]  && add_to_path "$HOME/.bash_profile"
    [ -f "$HOME/.zshrc" ]         && add_to_path "$HOME/.zshrc"
    # Fall back to .profile if none of the above exist
    if [ ! -f "$HOME/.bashrc" ] && [ ! -f "$HOME/.bash_profile" ] && [ ! -f "$HOME/.zshrc" ]; then
        add_to_path "$HOME/.profile"
    fi
    echo "Run this to apply immediately:  export PATH=\"\$HOME/.local/bin:\$PATH\""
fi

echo "Setting up systemd user service for $SERVICE_NAME..."
mkdir -p "$(dirname "$SERVICE_FILE")"

cat > "$SERVICE_FILE" <<EOL
[Unit]
Description=DeskConn Daemon
After=network.target

[Service]
ExecStart=$EXEC_DIR/deskconnd
Restart=always
RestartSec=5
Environment=TERM=xterm-256color

[Install]
WantedBy=default.target
EOL

systemctl --user daemon-reload

if systemctl --user is-enabled --quiet "$SERVICE_NAME"; then
    echo "Service exists. Restarting..."
    systemctl --user restart "$SERVICE_NAME"
else
    echo "Enabling and starting service..."
    systemctl --user enable "$SERVICE_NAME"
    systemctl --user start "$SERVICE_NAME"
fi
echo "Systemd service $SERVICE_NAME installed and started!"
