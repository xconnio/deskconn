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

if [ "$OS" = "darwin" ] && [ "$GO_ARCH" != "arm64" ]; then
    echo "Prebuilt macOS releases support Apple Silicon (arm64) only." >&2
    exit 1
fi

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

rm -f "$BIN_DIR/desk"

mv "$TMP_DIR/deskconn" "$EXEC_DIR/deskconn"
chmod 755 "$EXEC_DIR/deskconn"
ln -sf "$EXEC_DIR/deskconn" "$BIN_DIR/desk"

# desk generates its completion scripts and applies the zsh/bash fixes itself, the same
# way "desk self update" does, so the installer and updates can't drift apart.
if "$BIN_DIR/desk" self install-completions; then
    echo "Installed shell completions"
else
    echo "warning: failed to install shell completions" >&2
fi
echo "Installed desk $VERSION"

# BIN_DIR goes into shell config files only if it isn't on PATH already.
NEED_PATH=""
case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *) NEED_PATH=1 ;;
esac

# Add BIN_DIR (when needed) and shell completions to shell config files if not already present
add_shell_config() {
    local file="$1"
    local shell_name="$2"

    if [ -n "$NEED_PATH" ]; then
        grep -qF '.local/bin' "$file" 2>/dev/null || {
            printf '\n# Added by deskconn installer\nexport PATH="$HOME/.local/bin:$PATH"\n' >> "$file"
            echo "  Updated $file with PATH"
        }
    fi

    if [ "$shell_name" = "zsh" ]; then
        grep -qF 'deskconn completion' "$file" 2>/dev/null || {
            cat >> "$file" << 'ZSHEOF'

# Added by deskconn installer (deskconn completion)
if [ -d "$HOME/.local/share/zsh/site-functions" ]; then
    fpath=("$HOME/.local/share/zsh/site-functions" $fpath)
    autoload -Uz _desk 2>/dev/null
    compdef _desk desk deskconn 2>/dev/null
fi
ZSHEOF
            echo "  Updated $file with shell completions"
        }
    elif [ "$shell_name" = "bash" ]; then
        grep -qF 'deskconn completion' "$file" 2>/dev/null || {
            cat >> "$file" << 'BASHEOF'

# Added by deskconn installer (deskconn completion)
if [ -f "$HOME/.local/share/bash-completion/completions/desk" ]; then
    . "$HOME/.local/share/bash-completion/completions/desk" 2>/dev/null
fi
BASHEOF
            echo "  Updated $file with shell completions"
        }
    fi
}

[ -f "$HOME/.bashrc" ]        && add_shell_config "$HOME/.bashrc" "bash"
[ -f "$HOME/.bash_profile" ]  && add_shell_config "$HOME/.bash_profile" "bash"
[ -f "$HOME/.zshrc" ]         && add_shell_config "$HOME/.zshrc" "zsh"
# Fall back to .profile if none of the above exist (.zshrc on macOS, where zsh is
# the default shell and doesn't read .profile)
if [ ! -f "$HOME/.bashrc" ] && [ ! -f "$HOME/.bash_profile" ] && [ ! -f "$HOME/.zshrc" ]; then
    if [ "$OS" = "darwin" ]; then
        # A .zshrc we create ourselves has nothing else to run compinit, without which
        # compdef doesn't exist and completion silently does nothing.
        printf '# Added by deskconn installer\nautoload -Uz compinit && compinit\n' > "$HOME/.zshrc"
        add_shell_config "$HOME/.zshrc" "zsh"
    else
        add_shell_config "$HOME/.profile" "sh"
    fi
fi
if [ -n "$NEED_PATH" ]; then
    echo "Run this to apply immediately:  export PATH=\"\$HOME/.local/bin:\$PATH\""
fi

install_service_linux() {
    local systemd_user_dir="$HOME/.config/systemd/user"

    # systemd --user services don't reliably inherit DISPLAY/WAYLAND_DISPLAY from
    # the desktop session, so the daemon can't tell a desktop from a headless
    # server apart without them being exported explicitly. Capture them from the
    # installer's own environment: on a real desktop session they'll be set here;
    # on a headless server they won't, and the daemon will register server-only
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

    if [ ! -d /run/systemd/system ]; then
        echo "systemd is not running here, skipping service setup."
        echo "Start the daemon manually with:  $EXEC_DIR/deskconn serve --mode user"
        return
    fi

    # Shells without a login session (root in a container, "su" without "-l", ...)
    # have no user bus. Enable lingering so this user's systemd instance runs
    # (and starts at boot without a login), then point systemctl at it.
    if [ -z "${XDG_RUNTIME_DIR:-}" ] && [ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]; then
        echo "No user session found, enabling lingering for $(id -un)..."
        loginctl enable-linger "$(id -un)"
        export XDG_RUNTIME_DIR="/run/user/$(id -u)"
        for _ in $(seq 1 20); do
            [ -S "$XDG_RUNTIME_DIR/bus" ] && break
            sleep 0.5
        done
    fi

    mkdir -p "$systemd_user_dir"

    local unit_file="$systemd_user_dir/deskconn.service"
    local drop_ins="$systemd_user_dir/deskconn.service.d"
    local legacy_unit_file="$systemd_user_dir/deskconn-userd.service"
    local legacy_drop_ins="$systemd_user_dir/deskconn-userd.service.d"

    # The daemon ran as deskconn-userd before. Keep what that service had: the environment
    # captured when it was installed (a desktop's DISPLAY, say, while this runs over ssh), and
    # its drop-ins, with the flags and variables added to it. Its unit stays until deskconn
    # runs, to go back to if it doesn't.
    #
    # Both run the daemon, which can't run twice, so deskconn-userd must stop before deskconn
    # starts. Stopping it here would also stop this script when it runs in a desk shell on this
    # machine (it's in deskconn-userd's cgroup then), leaving neither running. Instead, a
    # drop-in makes deskconn conflict with deskconn-userd, so the one start below stops it first,
    # which systemd carries out even if this script is stopped along the way; the daemon then
    # removes deskconn-userd's leftovers itself.
    local migrating=""
    if [ -f "$legacy_unit_file" ]; then
        migrating=1
        echo "Renaming the deskconn-userd service to deskconn..."
        local line name
        while IFS= read -r line || [ -n "$line" ]; do
            case "$line" in
                Environment=*)
                    name="${line#Environment=}"
                    name="${name#\"}"
                    name="${name%%=*}"
                    case "$userd_env_lines" in
                        *"Environment=$name="*) ;;
                        *) userd_env_lines="$userd_env_lines
$line" ;;
                    esac
                    ;;
            esac
        done < "$legacy_unit_file"
        if [ -d "$legacy_drop_ins" ]; then
            mkdir -p "$drop_ins"
            # A drop-in that's a symlink (to a dotfiles repository, say) stays one: -P.
            for drop_in in "$legacy_drop_ins"/*; do
                local dest
                dest="$drop_ins/$(basename "$drop_in")"
                if { [ -f "$drop_in" ] || [ -L "$drop_in" ]; } && [ ! -e "$dest" ] && [ ! -L "$dest" ]; then
                    cp -P "$drop_in" "$dest"
                fi
            done
        fi
        mkdir -p "$drop_ins"
        printf '[Unit]\nConflicts=deskconn-userd.service\nAfter=deskconn-userd.service\n' \
            > "$drop_ins/50-migrate-from-deskconn-userd.conf"
    fi

    cat > "$unit_file" <<EOL
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

    if [ -n "$migrating" ]; then
        systemctl --user disable deskconn-userd 2>/dev/null || true
        if ! systemctl --user enable deskconn || ! systemctl --user restart deskconn; then
            echo "The deskconn service didn't start, so deskconn-userd was kept." >&2
            systemctl --user stop deskconn 2>/dev/null || true
            systemctl --user disable deskconn 2>/dev/null || true
            rm -f "$unit_file" "$drop_ins/50-migrate-from-deskconn-userd.conf"
            systemctl --user daemon-reload
            systemctl --user enable deskconn-userd 2>/dev/null || true
            systemctl --user start deskconn-userd 2>/dev/null || true
            exit 1
        fi
        rm -f "$legacy_unit_file" "$drop_ins/50-migrate-from-deskconn-userd.conf"
        rm -rf "$legacy_drop_ins"
        systemctl --user daemon-reload
        systemctl --user reset-failed deskconn-userd 2>/dev/null || true
    elif systemctl --user is-enabled --quiet deskconn; then
        echo "Service deskconn exists. Restarting..."
        systemctl --user restart deskconn
    else
        echo "Enabling and starting deskconn..."
        systemctl --user enable deskconn
        systemctl --user start deskconn
    fi
    echo "Systemd service deskconn installed and started!"
}

# The user daemon runs as a per-user launchd agent in the GUI session - the macOS
# equivalent of "systemctl --user" on Linux and the logon scheduled task on Windows.
install_service_darwin() {
    local label="com.deskconn.deskconn"
    local legacy_label="com.deskconn.deskconn-userd"
    local domain="gui/$(id -u)"
    local plist_file="$HOME/Library/LaunchAgents/$label.plist"
    local legacy_plist_file="$HOME/Library/LaunchAgents/$legacy_label.plist"
    local log_dir="$HOME/Library/Logs/deskconn"

    echo "Setting up launchd agent $label..."
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
        <string>$EXEC_DIR/deskconn</string>
        <string>serve</string>
        <string>--mode</string>
        <string>user</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>EnvironmentVariables</key>
    <dict>
        <key>TERM</key>
        <string>xterm-256color</string>
        <!-- launchd's own PATH lacks Homebrew's, where ffmpeg (video thumbnails) lives. -->
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>$log_dir/deskconn.log</string>
    <key>StandardErrorPath</key>
    <string>$log_dir/deskconn.log</string>
</dict>
</plist>
EOL

    # The agent was $legacy_label before. Keep the variables added to it; its plist stays
    # until $label loads, to go back to if it doesn't.
    local migrating=""
    if [ -f "$legacy_plist_file" ]; then
        migrating=1
        echo "Renaming the $legacy_label agent to $label..."
        local legacy_env
        legacy_env="$(mktemp)"
        if plutil -extract EnvironmentVariables xml1 -o "$legacy_env" "$legacy_plist_file" 2>/dev/null; then
            # Adds only the variables the new plist doesn't set itself.
            /usr/libexec/PlistBuddy -c "Merge $legacy_env :EnvironmentVariables" "$plist_file" >/dev/null 2>&1 || true
        fi
        rm -f "$legacy_env"
    fi

    if ! launchctl print "$domain" >/dev/null 2>&1; then
        # Not loaded now, so nothing to stop: only one of them may load at next login.
        rm -f "$legacy_plist_file"
        echo "No GUI session for $(id -un), so the agent wasn't loaded now. It will start at next login."
        echo "Start the daemon manually with:  $EXEC_DIR/deskconn serve --mode user"
        return
    fi

    # Unload a running agent first so the rewritten plist (and the new binary) are what
    # gets loaded; bootout returns before the agent is fully gone, and bootstrapping it
    # again before then fails.
    # The old agent too, as both run the daemon, which can't run twice.
    local running
    for running in "$label" "$legacy_label"; do
        if launchctl print "$domain/$running" >/dev/null 2>&1; then
            echo "Agent $running exists. Stopping it..."
            launchctl bootout "$domain/$running" 2>/dev/null || true
            for _ in 1 2 3 4 5 6 7 8 9 10; do
                launchctl print "$domain/$running" >/dev/null 2>&1 || break
                sleep 0.5
            done
        fi
    done
    echo "Enabling and starting $label..."
    rm -f "$FULL_DISK_ACCESS_FILE"
    launchctl enable "$domain/$label"
    local loaded=""
    for _ in 1 2 3 4 5; do
        if launchctl bootstrap "$domain" "$plist_file" 2>/dev/null; then
            loaded=1
            break
        fi
        sleep 1
    done
    if [ -z "$loaded" ] && [ -n "$migrating" ]; then
        echo "Couldn't load $label, so $legacy_label was kept." >&2
        rm -f "$plist_file"
        launchctl enable "$domain/$legacy_label" 2>/dev/null || true
        launchctl bootstrap "$domain" "$legacy_plist_file" 2>/dev/null || true
        exit 1
    fi
    if [ -z "$loaded" ]; then
        echo "Couldn't load $label (the old agent may still be exiting). It will start at next login."
        echo "Start it now with:  launchctl bootstrap $domain $plist_file"
        return
    fi
    if [ -n "$migrating" ]; then
        launchctl disable "$domain/$legacy_label" 2>/dev/null || true
        rm -f "$legacy_plist_file"
    fi
    echo "launchd agent $label installed and started - deskconn will run in your session at login!"

    request_full_disk_access "$domain/$label"
}

# The daemon records here, at startup, whether it has Full Disk Access ("granted" or
# "missing"). Only it can tell: macOS decides that for the process asking, so checking from
# this script would test the terminal's access instead.
FULL_DISK_ACCESS_FILE="$HOME/.deskconn/full-disk-access"

# full_disk_access_status waits for the daemon to (re)write FULL_DISK_ACCESS_FILE, then
# prints its status, or "unknown" if the daemon didn't get that far.
full_disk_access_status() {
    for _ in $(seq 1 20); do
        if [ -s "$FULL_DISK_ACCESS_FILE" ]; then
            cat "$FULL_DISK_ACCESS_FILE"
            return
        fi
        sleep 0.5
    done
    echo unknown
}

# Without Full Disk Access, macOS asks separately the first time the daemon reads each
# protected place (Desktop, Documents, Downloads, external and network volumes, other apps'
# data), and a command-line tool can't ask for those up front. One Full Disk Access grant
# covers them all, but only the user can switch it on, in System Settings: open it at the
# right page, with deskconn revealed in Finder to drag into the list.
request_full_disk_access() {
    local target="$1"
    [ "$(full_disk_access_status)" = "missing" ] || return 0

    echo
    echo "deskconn needs Full Disk Access to reach your files without macOS asking about each folder."
    echo "In System Settings > Privacy & Security > Full Disk Access, click + (or drag it in from Finder)"
    echo "and add:  $EXEC_DIR/deskconn"
    open "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles" 2>/dev/null || true
    open -R "$EXEC_DIR/deskconn" 2>/dev/null || true

    # "curl ... | sh" leaves stdin as the script, so ask on the terminal, if there is one.
    if ! true 2>/dev/null </dev/tty; then
        echo "Then restart it with:  launchctl kickstart -k $target"
        return 0
    fi
    exec 3</dev/tty
    while :; do
        printf "Press Enter once it's switched on (or type s to skip): "
        local answer=""
        read -r answer <&3 || break
        [ "$answer" = "s" ] && break
        # Full Disk Access applies from the daemon's next start.
        rm -f "$FULL_DISK_ACCESS_FILE"
        launchctl kickstart -k "$target"
        if [ "$(full_disk_access_status)" != "missing" ]; then
            echo "Full Disk Access granted."
            break
        fi
        echo "deskconn still doesn't have Full Disk Access."
    done
    exec 3<&-
}

case "$OS" in
    linux)
        install_service_linux
        ;;
    darwin)
        install_service_darwin
        ;;
esac
