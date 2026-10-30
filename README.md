# Deskconn

A split operating system where the runtime lives on your computer, the interface lives on any device, and applications
can execute locally or in the cloud.

It lets you control Linux desktops remotely (run shells, transfer files, forward ports and more) from any machine on your
account. Connections are routed through the [Deskconn cloud router](https://github.com/xconnio/deskconn-router) and can
transparently upgrade to direct WebRTC P2P links for lower latency.

## Components

The Deskconn ecosystem consists of five pieces. This repository contains the two that run on the managed desktop:

| Component                                                                                                                               | Role                                                                                                                          |
|-----------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------|
| [deskconn-userd](https://github.com/xconnio/deskconn)                                                                                   | Desktop daemon. Registers and exposes desktop APIs over WAMP. Runs as a systemd user service.                                 |
| [desk](https://github.com/xconnio/deskconn)                                                                                             | Control CLI. Attach desktops, manage files, open shells, forward ports, and more.                                             |
| [deskconn-router](https://github.com/xconnio/deskconn-router)                                                                           | Cloud WAMP router. The central hub — every component (CLI, daemon, account service, web app, mobile app) connects through it. |
| [deskconn-account-service](https://github.com/xconnio/deskconn-account-service)                                                         | Manages user accounts, organizations, and per-machine CryptoSign principals.                                                  |
| [deskconn-web-app](https://github.com/xconnio/deskconn-web-app) / [deskconn-mobile-app](https://github.com/xconnio/deskconn-mobile-app) | Web and mobile interfaces.                                                                                                    |

### How a command reaches your desktop

```
desk CLI
    │  (Unix socket — ~/.deskconn/deskconn.sock)
    ▼
deskconn-userd (local proxy)
    │  (WebSocket — wss://api.deskconn.com/ws or WebRTC P2P)
    ▼
deskconn-userd (target machine)
```

The local `deskconn-userd` maintains a persistent session to the cloud router per target machine. The shell command starts
over the routed path and migrates transparently to a direct WebRTC connection in the background once the P2P handshake
completes.

### Authentication

All cloud connections use CryptoSign (Ed25519). `desk account login` generates a keypair, registers the public key with
the account service, and stores the private key in `~/.deskconn/id_ed25519`.

## Installation

**macOS and Linux**

```bash
curl -fsSL https://get.deskconn.com | sh
```

This installs one binary, `deskconn`, to `~/.local/lib/exec`, with `desk` on your `PATH` (`~/.local/bin/desk`) a
symlink to it. It's the CLI, and `desk serve --mode user|standalone|root` runs the daemons:

| Command                        | Role                                                                                      |
|--------------------------------|-------------------------------------------------------------------------------------------|
| `desk`                         | The CLI                                                                                   |
| `desk serve --mode user`       | User daemon (`deskconn-userd`), run by the `deskconn-userd` systemd user service at login |
| `desk serve --mode standalone` | User daemon serving the machine directly, without the cloud (see below)                   |
| `desk serve --mode root`       | Root helper (`deskconn-rootd`), started on demand with `sudo` as `deskconn-rootd.service` |

On macOS, the user daemon runs as the `com.deskconn.deskconn-userd` launchd agent in your login session instead
(logs in `~/Library/Logs/deskconn`), and there's no root helper (VPN serving is Linux-only for now).

On Windows, from PowerShell:

```powershell
irm https://get.deskconn.com/install.ps1 | iex
```

This installs the same binary to `%LOCALAPPDATA%\deskconn`, adds it to your `PATH`, and registers the
user daemon as a scheduled task that starts at logon (there's no root helper on Windows yet — VPN serving is
Linux-only for now). Use `deskconn` there instead of `desk` — `desk` is reserved by Windows for Display Settings
(`desk.cpl`) and can't be safely overridden.

## Getting started

1. Create an account at [deskconn.com](https://deskconn.com) or in the mobile app.
2. Attach the desktop to it. This writes `~/.deskconn/credentials.json`, which the daemon picks up to connect to the
   cloud:

   ```bash
   desk machine attach --username <email>
   ```

3. Log in from the CLI with the one-time password emailed to you. The key it creates is valid for 30 days:

   ```bash
   desk account login --username <username>
   ```

4. Use your machines:

   ```bash
   desk top               # your machines and profiles: resources, logs, shells, port forwards
   desk ls
   desk shell <machine>
   ```

Run `desk --help` for the other commands (files, ports, VPN, agents, ...) and `desk <command> --help` for any one of
them.

## Standalone mode (no cloud)

`desk serve --mode standalone` serves a machine directly, without an account or the cloud router, to the users you
list: with a password, or with a key from `desk keygen`. Shell, files, ports, agents, logs and P2P work as usual.

On the machine, write a config, add your users to it, and serve it. It logs the certificate's fingerprint for clients
to pin:

```bash
desk serve init                   # writes ./deskconn.yaml, with every setting explained
desk serve --config deskconn.yaml
```

On the client, save the machine as a profile and use it with `--profile`:

```bash
desk profile add office --url quic://203.0.113.5:18080 --cert-hash <fingerprint> --private-key <key>
desk --profile office shell
```

Leave out `--private-key` to log in with a password instead (`--authid <username>`). `desk serve run --help` and
`desk profile add --help` cover the rest, such as serving over WebTransport, using your own TLS certificate, and
passing users as flags instead of a config file.

Users can also be added and removed while the machine is served, by its admins: the config file's users with
`role: admin`. They do it through the `io.xconn.deskconn.admin.users.list`, `.add` and `.remove` procedures. The
users added are members, kept in `~/.deskconn/standalone-users.db` beside the config file's users, which only the
config file changes.

The procedures take plain arguments, so any WAMP client can call them. For clients that only speak WebSocket,
standalone mode also serves `ws://127.0.0.1:18082/ws`: plain, without TLS, so only on localhost.

## Self-hosting

Run your own cloud (router, account service, web app and database) locally with the Docker Compose setup in
[`docker/`](docker), then build `deskconn` pointed at it.

### 1. Start the stack

```bash
cd docker
make setup
make run
cd ..
```

### 2. Build the binaries against it

```bash
make build CLOUD_QUIC_ADDRESS=127.0.0.1:8081   # builds bin/deskconn and bin/desk
```

`CLOUD_QUIC_ADDRESS` is the router's QUIC address, baked in as the default. For a stack on another machine, use its
`host:port`. Local addresses (`127.0.0.1`, `localhost`, `::1`) skip TLS verification, because the local router uses a
self-signed certificate; any other host needs a router certificate the client trusts.

Setting `DESKCONN_CLOUD_QUIC_ADDRESS` at runtime overrides the built-in address without rebuilding:

```bash
DESKCONN_CLOUD_QUIC_ADDRESS=127.0.0.1:8081 desk ls
```

WebRTC uses a public STUN server only. To also relay through your own TURN server, set it in the environment.
`DESKCONN_TURN_URL` accepts a comma-separated list:

```bash
export DESKCONN_TURN_URL=turn:turn.example.com:3478 DESKCONN_TURN_USERNAME=<user> DESKCONN_TURN_PASSWORD=<password>
```

The deskconn-userd service doesn't inherit your shell's environment; add the variables to it with
`systemctl --user edit deskconn-userd`:

```ini
[Service]
Environment=DESKCONN_TURN_URL=turn:turn.example.com:3478
Environment=DESKCONN_TURN_USERNAME=<user>
Environment=DESKCONN_TURN_PASSWORD=<password>
```

### 3. Create an account and attach

Sign up at http://localhost:3000/register. The stack prints one-time passwords to its logs instead of emailing them:

```bash
docker compose logs -f account-service   # in docker/
```

Then follow [Getting started](#getting-started) from step 2 with the binaries from `bin/`, and run the daemon in the
foreground:

```bash
systemctl --user stop deskconn-userd   # if the official release is installed; it shares ~/.deskconn
bin/desk machine attach --username <email>
bin/desk serve
```

Don't run `desk self update` on these builds: it installs the official release, which connects to
`api.deskconn.com`.

## Adding an app

Apps beyond deskconn-userd's own (Files, Terminal, Resource Monitor, Text Editor) run as their own processes, like
[media-app](https://github.com/xconnio/media-app) and [remotecontrol-app](https://github.com/xconnio/remotecontrol-app).
An app needs no code in deskconn: it connects to deskconn-userd's local socket, registers its procedures there, and
tells deskconn-userd which apps it serves. deskconn-userd lists them to clients (the web app shows an app only while
it's listed and enabled), owners and admins enable or disable them, and xconn exposes their procedures to the cloud
and P2P.

1. **Connect** to `~/.deskconn/deskconn.sock` (WAMP over a Unix socket, realm `io.xconn.deskconn.local`, anonymous).
   Reconnect when deskconn-userd restarts.
2. **Register a key exchange.** deskconn only supports end-to-end encrypted apps: deskconn-userd refuses an app without one.
   Clients call it with their X25519 public key and get the app's; both sides derive their keys with HKDF-SHA256,
   `frontendToBackend` for requests and `backendToFrontend` for results. Every other procedure takes its request as a
   single encrypted JSON argument (nonce + ChaCha20-Poly1305 ciphertext, empty or absent when there is none) and returns
   a single encrypted JSON result. Keys are per caller session.
3. **Register the apps** by calling `io.xconn.local.apps.register` with a JSON list:

   ```json
   [{"id": "pictures", "name": "Pictures", "key_exchange": "io.xconn.media.key.exchange",
     "requires": ["files"], "default_off": true}]
   ```

   `requires` are apps this one can't work without (it's disabled with them), and a `default_off` app stays disabled
   until an owner or admin enables it. Ids must be unique on the machine. The apps are listed until the app's session
   leaves.
4. **Follow enabled apps.** Call `io.xconn.deskconn.capabilities.list` for every app's
   `{"id", "name", "enabled", "key_exchange", "requires"}`, and subscribe to
   `io.xconn.deskconn.capabilities.on_change`, published with the same list whenever an app is enabled or
   disabled. Register an app's procedures while it's enabled and unregister them while it's disabled; keep the key
   exchange registered.

Procedures under `io.xconn.deskconn.` are reachable by remote clients (and `io.xconn.deskconn.admin.` by
the machine's owner and admins only), and so are the app prefixes the cloud router allows (`relay.AppPrefixes`:
`io.xconn.remotecontrol.` and `io.xconn.media.`), so the key exchange and the app's procedures go under one of them.
Everything else on the socket, such as `io.xconn.local.*`, stays local.

In Go, the `github.com/xconnio/deskconn/appkit` package has what an app needs: the socket (`appkit.SocketURI`), the
procedures above, and `appkit.Encryption`, which does step 2:

```go
session, err := xconn.ConnectAnonymous(ctx, appkit.SocketURI(cfgDirectory), appkit.LocalRealm)
enc := appkit.NewEncryption()
if err := enc.Register(session, "io.xconn.media.key.exchange"); err != nil {
    return err
}
handler := enc.Encrypted(func(ctx context.Context, req []byte) (any, error) {
    // req is the decrypted JSON request; the result is sent back as encrypted JSON.
    return map[string]string{"hello": "world"}, nil
})
resp := session.Register("io.xconn.media.hello", handler).Do()
```

A client finds the app's key exchange in the apps list, then calls it before the app's procedures:

```go
keys, err := appkit.ClientKeyExchange(session, keyExchange)
result, err := appkit.EncryptedCall(session, "io.xconn.media.hello", nil, keys)
```

## Development

### Build

```bash
make build   # builds bin/deskconn and bin/desk
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

| File                    | Contents                                                                |
|-------------------------|-------------------------------------------------------------------------|
| `credentials.json`      | Machine attach credentials (realm, authid, keypair) used by `deskconn-userd` |
| `id_ed25519`            | CLI private key, username, and key expiry                               |
| `id_ed25519.pub`        | CLI public key, username, and account name                              |
| `config.yml`            | Machine list and aliases, and standalone machine profiles (`desk profile add`) with their private keys |
| `standalone-users.db`   | Standalone mode's users added through its API, with their keys and salted password hashes |
| `principals.json`       | Local CryptoSign principals (used by the local WAMP router)             |
| `turn_credentials.json` | Cached TURN server credentials for WebRTC                               |
| `deskconn.sock`         | Unix socket for local CLI–daemon communication                          |

## License

See [LICENSE](LICENSE).
