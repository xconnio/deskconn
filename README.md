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

```bash
curl -fsSL https://get.deskconn.com | sh
```

This installs `deskconn` and `deskconnd` to `~/.local/bin` and registers `deskconnd` as a systemd user service that
starts automatically.

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
deskconn login --username <email> --password <password>
```

Generates an Ed25519 keypair, registers it with the account service, and stores it locally. You only need to do this
once per machine; the key is valid for 30 days and is renewed on the next login.

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
| `config.yml`            | Device list and aliases                                                |
| `principals.json`       | Local CryptoSign principals (used by the local WAMP router)            |
| `turn_credentials.json` | Cached TURN server credentials for WebRTC                              |
| `deskconn.sock`         | Unix socket for local CLI–daemon communication                         |

## License

See [LICENSE](LICENSE).
