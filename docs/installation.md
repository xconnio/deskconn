# Installation and local configuration

```bash
# On macOS and Linux.
curl -fsSL https://get.deskconn.com/install.sh | sh
```

```powershell
# On Windows.
irm https://get.deskconn.com/install.ps1 | iex
```

On Linux, this installs one binary, `deskconn`, to `~/.local/lib/exec`, with `desk` on your `PATH`
(`~/.local/bin/desk`) a symlink to it. It's the CLI, and `desk serve --mode user|standalone|root` runs the daemons:

| Command                        | Role                                                                                      |
|--------------------------------|-------------------------------------------------------------------------------------------|
| `desk`                         | The CLI                                                                                   |
| `desk serve --mode user`       | User daemon (`deskconn-userd`), run by the `deskconn-userd` systemd user service at login |
| `desk serve --mode standalone` | User daemon serving the machine directly, without the cloud (see below)                   |
| `desk serve --mode root`       | Root helper (`deskconn-rootd`), started on demand with `sudo` as `deskconn-rootd.service` |

On macOS, the user daemon runs as the
`com.deskconn.deskconn-userd` launchd agent in your login session, with logs in
`~/Library/Logs/deskconn`. The root helper and VPN serving are unavailable on macOS.
DeskConn needs Full Disk Access (System Settings > Privacy & Security > Full Disk Access):
without it, macOS asks separately the first time the daemon reads each protected folder (Desktop, Documents,
Downloads, external drives, ...).

On Windows, this installs the same binary to `%LOCALAPPDATA%\deskconn`, along with the [Wintun](https://www.wintun.net)
driver that `deskconn vpn connect` uses, adds it to your `PATH`, and registers the user daemon as a scheduled task that
starts at logon. Use `deskconn` there instead of `desk` — `desk` is reserved by Windows for Display Settings
(`desk.cpl`) and can't be safely overridden.

`deskconn vpn connect` brings up a UAC prompt, and while a tunnel is up Windows asks the tunnel's DNS resolvers first
(a Linux exit node's own, or public resolvers through a Windows one), though its smart multi-homed name resolution may
still send lookups to other adapters' resolvers too. `deskconn vpn serve` needs no prompt or driver: it runs inside the
daemon, as a userspace NAT that carries TCP, UDP and ping.

## Authentication

All cloud connections use CryptoSign (Ed25519). `desk account login` generates a
keypair, registers the public key with the management service, and stores the
private key in `~/.deskconn/id_ed25519`.

## Credential files

All credentials and configuration are stored under `~/.deskconn/`:

| File                    | Contents                                                                |
|-------------------------|-------------------------------------------------------------------------|
| `credentials.json`      | Machine attach credentials (realm, authid, keypair) used by `deskconn-userd` |
| `id_ed25519`            | CLI private key, username, and key expiry                               |
| `id_ed25519.pub`        | CLI public key, username, and account name                              |
| `config.yml`            | Machine list and aliases, and standalone machine profiles (`desk profile add`) with their private keys |
| `standalone-users.db`   | Standalone mode's users added through its API, with their keys and salted password hashes |
| `principals.json`       | Local CryptoSign principals (used by the local WAMP router)             |
| `capabilities.json`     | Which apps the machine's owner and admins have enabled or disabled      |
| `manifests/`            | Registered manifest apps                                                |
| `settings.db`           | Each user's frontend settings kept on the machine (open windows, dock)  |
| `deskconn.sock`         | Local Unix socket, or private Windows endpoint credentials              |

