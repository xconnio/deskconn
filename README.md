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

`deskconnd` can serve a device directly, without an account or the cloud router, to a fixed list of keys. One command
starts it; clients point the CLI at its URL. Shell, exec, file operations, port and agent forwarding, logs and P2P
work the same way.

### 1. Generate a key pair

```bash
deskconn keygen
# Public Key:  65160c38…
# Private Key: 9f2d0b17…
```

### 2. Start deskconnd in standalone mode on the device

```bash
deskconnd --standalone --url tcp://0.0.0.0:18080 --public-key 65160c38… [--public-key <another public key> ...]
```

`--url` is `tcp://host:port` or `unix:///path/to.sock` (default `tcp://0.0.0.0:18080`). Clients holding one of the
`--public-key`s can connect, whatever authid they present. To run it as the service, put the flags on `ExecStart` with
`systemctl --user edit deskconnd`.

### 3. Connect from the client

```bash
export DESKCONN_URL=tcp://203.0.113.5:18080 DESKCONN_PRIVATE_KEY=9f2d0b17…
deskconn shell
deskconn exec -- uname -a
deskconn file cp ./notes.txt :/home/me/notes.txt   # remote paths start with ':'
deskconn file cat :/etc/hostname
deskconn port forward 8080:80
```

The same works with flags: `deskconn --url tcp://203.0.113.5:18080 --private-key 9f2d0b17… shell`. With `--url`,
commands take no device argument. `ping`, `connect`, `disconnect` and `ls` work on devices of your account only.

## Self-hosting

Run your own cloud (router, account service, web app and database) locally with
[deskconn-docker](https://github.com/xconnio/deskconn-docker), then build `deskconn` and `deskconnd` pointed at it.

### 1. Start the stack

```bash
git clone https://github.com/xconnio/deskconn-docker
cd deskconn-docker
make setup
make run
```

### 2. Build the binaries against it

```bash
make build CLOUD_QUIC_ADDRESS=127.0.0.1:8081   # builds bin/deskconn and bin/deskconnd
```

`CLOUD_QUIC_ADDRESS` is the router's QUIC address, baked in as the default. For a stack on another machine, use its
`host:port`. Local addresses (`127.0.0.1`, `localhost`, `::1`) skip TLS verification, because the local router uses a
self-signed certificate; any other host needs a router certificate the client trusts.

Setting `DESKCONN_CLOUD_QUIC_ADDRESS` at runtime overrides the built-in address without rebuilding:

```bash
DESKCONN_CLOUD_QUIC_ADDRESS=127.0.0.1:8081 deskconn ls
```

### 3. Create an account and attach

Sign up at http://localhost:3000/register. The stack prints one-time passwords to its logs instead of emailing them:

```bash
docker compose logs -f account-service   # in deskconn-docker
```

Then follow [Getting started](#getting-started) from step 2 with the binaries from `bin/`, and run the daemon in the
foreground:

```bash
systemctl --user stop deskconnd   # if the official release is installed; it shares ~/.deskconn
bin/deskconn attach --username <email> --password <password>
bin/deskconnd
```

Don't run `deskconn self update` on these builds: it installs the official release, which connects to
`api.deskconn.com`.

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
deskconn keygen                                        # key pair: public for deskconnd --public-key, private for --private-key
deskconn --url <tcp://host:port> --private-key <hex> <command>   # also DESKCONN_URL / DESKCONN_PRIVATE_KEY
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
make build-deskconnd   # builds bin/deskconnd
make build-deskconn    # builds bin/deskconn
```

Override the cloud router for local development (see [Self-hosting](#self-hosting)):

```bash
export DESKCONN_CLOUD_QUIC_ADDRESS=127.0.0.1:8081
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
