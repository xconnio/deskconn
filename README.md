# Deskconn

A split operating system where the runtime lives on your computer, the interface lives on any device, and applications
can execute locally or in the cloud.

It lets you control Linux desktops remotely (run shells, transfer files, forward ports and more) from any device on your
account. Connections are routed through the [Deskconn cloud router](https://github.com/xconnio/deskconn-router) and can
transparently upgrade to direct WebRTC P2P links for lower latency.

## Components

The Deskconn ecosystem consists of five pieces. This repository contains the two that run on the managed desktop:

| Component                                                                                                                               | Role                                                                                                                          |
|-----------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------|
| [deskconnd](https://github.com/xconnio/deskconn)                                                                                        | Desktop daemon. Registers and exposes desktop APIs over WAMP. Runs as a systemd user service.                                 |
| [deskconn](https://github.com/xconnio/deskconn)                                                                                         | Control CLI. Attach desktops, manage files, open shells, forward ports, and more.                                             |
| [deskconn-router](https://github.com/xconnio/deskconn-router)                                                                           | Cloud WAMP router. The central hub — every component (CLI, daemon, account service, web app, mobile app) connects through it. |
| [deskconn-account-service](https://github.com/xconnio/deskconn-account-service)                                                         | Manages user accounts, organizations, and per-device CryptoSign principals.                                                   |
| [deskconn-web-app](https://github.com/xconnio/deskconn-web-app) / [deskconn-mobile-app](https://github.com/xconnio/deskconn-mobile-app) | Web and mobile interfaces.                                                                                                    |

### How a command reaches your desktop

```
deskconn CLI
    │  (Unix socket — ~/.deskconn/deskconn.sock)
    ▼
deskconnd (local proxy)
    │  (WebSocket — wss://api.deskconn.com/ws or WebRTC P2P)
    ▼
deskconnd (target device)
```

The local `deskconnd` maintains a persistent session to the cloud router per target device. The shell command starts
over the routed path and migrates transparently to a direct WebRTC connection in the background once the P2P handshake
completes.

### Authentication

All cloud connections use CryptoSign (Ed25519). `deskconn login` generates a keypair, registers the public key with the
account service, and stores the private key in `~/.deskconn/id_ed25519`.

## Installation

**macOS and Linux**

```bash
curl -fsSL https://get.deskconn.com | sh
```

**Windows** (PowerShell)

```powershell
irm https://get.deskconn.com/install.ps1 | iex
```

Each installs `deskconn` (the CLI) and `deskconnd` (the desktop daemon), adds `deskconn` to your `PATH`, and starts
`deskconnd` automatically in the background — as a systemd user service on Linux, a launchd agent on macOS, and a
scheduled task on Windows. On Windows, use `deskconn` or `desk-cli` — not `desk`, which is reserved for Display
Settings (`desk.cpl`).

## Getting started

### 1. Create an account

Sign up at [deskconn.com](https://deskconn.com) or via the mobile app.

### 2. Attach the desktop to the cloud

```bash
deskconn attach --username <email> --password <password>
# or read the password from stdin
echo "$PASSWORD" | deskconn attach --username <email> --password-stdin
```

This creates a realm for the desktop under your account and writes credentials to `~/.deskconn/credentials.json`. The
daemon picks these up automatically and connects to the cloud router.

### 3. Log in from the CLI

```bash
deskconn login --username <username> --password <password>
```

You'll then be prompted for the one-time password emailed to you. Once verified, deskconn generates an Ed25519
keypair, registers it with the account service, and stores it locally. You only need to do this once per machine;
the key is valid for 30 days and is renewed on the next login.

### 4. List your devices

```bash
deskconn ls
deskconn ls --refresh    # fetch the current list from the cloud
deskconn ls --detailed   # show realm, ID, and organisation
```

### 5. Open a shell

```bash
deskconn shell <device>
deskconn shell <device> --mode p2p      # force WebRTC
deskconn shell <device> --mode routed   # force cloud router
```

## Standalone mode (no cloud)

A device with a reachable IP (for example a cloud server) can be used without an account or the cloud router. Its
`xlink` serves the device directly over QUIC (UDP) to a fixed list of keys, and clients connect to it by address.
Shell, exec, file operations, port and agent forwarding, logs, P2P and VPN all work the same way.

### 1. Get the client's key

On each machine that should have access:

```bash
deskconn device key
# alice:65160c38…
```

### 2. Enable standalone mode on the device

Either with environment variables on the `xlink` service:

```bash
systemctl --user edit xlink
```

```ini
[Service]
Environment=DESKCONN_STANDALONE=1
Environment=DESKCONN_STANDALONE_KEYS=alice:65160c38…,bob:9a1f…
```

or in `~/.deskconn/config.yml`:

```yaml
standalone:
  enabled: true
  listen: 0.0.0.0:18080   # optional, this is the default
  principals:
    - authid: alice
      authorized_keys: [65160c38…]
```

Keys from both places are combined. Then restart the services and read the certificate fingerprint:

```bash
systemctl --user restart xlink deskconnd
journalctl --user -u xlink | grep fingerprint
# … add on a client with: deskconn device add <name> <public-ip>:18080 --fingerprint sha256:…
```

Allow **UDP** port 18080 through the device's firewall or cloud security group.

| Variable                     | Meaning                                                |
|------------------------------|--------------------------------------------------------|
| `DESKCONN_STANDALONE`        | `1`/`true` to enable, `0`/`false` to disable           |
| `DESKCONN_STANDALONE_LISTEN` | `host:port` to listen on (default `0.0.0.0:18080`)     |
| `DESKCONN_STANDALONE_KEYS`   | Comma-separated `authid:pubkey` entries to authorize   |

### 3. Add the device on the client

```bash
deskconn device add web1 203.0.113.5 --fingerprint sha256:…
deskconn shell web1
```

`device add` connects once to check the fingerprint and that the device accepts your key, then saves the device. The
port defaults to 18080.

### Security

- The client pins the device's certificate fingerprint and refuses to connect if it changes. The certificate is
  created on first start (`~/.deskconn/standalone.crt`) and kept across restarts; if it is replaced, re-add the device
  with the new fingerprint.
- Access is exactly the configured key list. To revoke a key, remove it and restart `xlink`.
- The client's key for standalone devices (`~/.deskconn/direct_ed25519`) is separate from the cloud login key: it
  does not expire and is kept on logout.

## CLI reference

### Account

```
deskconn login    [--username] [--password] [--password-stdin]
deskconn logout
deskconn whoami
deskconn attach   [--name] [--username] [--password] [--password-stdin]
deskconn detach   [--username] [--password] [--password-stdin]
```

### Devices

```
deskconn ls [--refresh] [--detailed]
deskconn ping <device> [--count N]
```

Standalone devices (see [Standalone mode](#standalone-mode-no-cloud)):

```
deskconn device add    <name> <host[:port]> --fingerprint <sha256:…>
deskconn device remove <name>
deskconn device key                         # this machine's key to authorize on a device
```

### Shell & exec

```
deskconn shell <device> [--mode p2p|routed] [-A]
deskconn exec  <device> <command...> [--p2p]
```

`dsh <device>` is a shortcut for `deskconn shell <device>`.

`-A`/`--agent-forward` forwards your local `ssh-agent` to the shell, like `ssh -A`, so tools run there (`git`,
`ssh`, ...) can authenticate with your local keys without copying them to the device. As with `ssh -A`, only use it
against devices you trust — anyone with access to the remote shell for the session's duration can ask the forwarded
agent to sign on your behalf.

### File operations

All file commands accept `device:path` for remote paths and a bare `/path` for local paths.

```
deskconn file ls  <device:path> [--mode p2p|routed]
deskconn file mv  <src> <dst>   [--mode p2p|routed]
deskconn file cp  <src> <dst>   [-r] [--mode p2p|routed]
deskconn file rm  <target>      [--mode p2p|routed]
deskconn file cat <device:path> [--mode p2p|routed]
```

`dcp <src> <dst>` is a shortcut for `deskconn file cp <src> <dst>`.

### Port forwarding

```
# Forward local:remote — traffic on localhost:LOCAL goes to REMOTE on the device
deskconn port forward <device> [-l LOCAL] [-r REMOTE] [--p2p]

# Reverse — the device listens on REMOTE and forwards to localhost:LOCAL
deskconn port reverse <device> [-r REMOTE] [-l LOCAL] [--p2p]
```

### Printing

```
deskconn print --enable [--host-printers]   # allow this desktop to receive print jobs
deskconn print --disable
deskconn print --status
deskconn print --ls <device>                # list printers on a device
deskconn print <device:printer> <file>      # send a print job
```

### Configuration

```
deskconn config show
deskconn config set <device> alias <value>
deskconn config unset <device> alias
deskconn config edit
```

### Self-update

```
deskconn self version
deskconn self update
```

## Development

### Build

```bash
make build-deskconnd   # builds ./deskconnd
make build-deskconn    # builds ./deskconn
```

Override the cloud endpoint for local development:

```bash
export DESKCONN_CLOUD_URI=ws://localhost:8080/ws
```

### Test

```bash
make test
```

## Credential files

All credentials and configuration are stored under `~/.deskconn/`:

| File                    | Contents                                                               |
|-------------------------|------------------------------------------------------------------------|
| `credentials.json`      | Device attach credentials (realm, authid, keypair) used by `deskconnd` |
| `id_ed25519`            | CLI private key, username, and key expiry                              |
| `id_ed25519.pub`        | CLI public key, username, and account name                             |
| `config.yml`            | Device list and aliases, standalone mode settings                      |
| `direct_ed25519`        | CLI private key and authid for standalone devices                      |
| `standalone.crt`/`.key` | Certificate a standalone device presents to clients                    |
| `principals.json`       | Local CryptoSign principals (used by the local WAMP router)            |
| `turn_credentials.json` | Cached TURN server credentials for WebRTC                              |
| `deskconn.sock`         | Unix socket for local CLI–daemon communication                         |

## License

See [LICENSE](LICENSE).
