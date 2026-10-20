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

**Linux**

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

On Windows, from PowerShell:

```powershell
irm https://get.deskconn.com/install.ps1 | iex
```

This installs the same binary to `%LOCALAPPDATA%\deskconn`, adds it to your `PATH`, and registers the
user daemon as a scheduled task that starts at logon (there's no root helper on Windows yet — VPN serving is
Linux-only for now). Use `deskconn` there instead of `desk` — `desk` is reserved by Windows for Display Settings
(`desk.cpl`) and can't be safely overridden.

## Getting started

### 1. Create an account

Sign up at [deskconn.com](https://deskconn.com) or via the mobile app.

### 2. Attach the desktop to the cloud

```bash
desk machine attach --username <email>
# or, without a prompt, read the password from stdin
echo "$PASSWORD" | desk machine attach --username <email>
```

This creates a realm for the desktop under your account and writes credentials to `~/.deskconn/credentials.json`. The
daemon picks these up automatically and connects to the cloud router.

### 3. Log in from the CLI

```bash
desk account login --username <username>
```

You'll then be prompted for the one-time password emailed to you. Once verified, desk generates an Ed25519
keypair, registers it with the account service, and stores it locally. You only need to do this once per machine;
the key is valid for 30 days and is renewed on the next login.

### 4. List your machines

```bash
desk ls
desk ls --refresh    # fetch the current list from the cloud
desk ls --detailed   # show realm, ID, and organisation
```

### 5. Open a shell

```bash
desk shell <machine>
desk shell <machine> --mode direct   # force WebRTC
desk shell <machine> --mode routed   # force cloud router
```

## Standalone mode (no cloud)

`deskconn-userd` can serve a machine directly, without an account or the cloud router, to a fixed list of keys and/or
username/password accounts. One command starts it; clients point the CLI at its URL. Shell, exec, file operations, port and agent forwarding, logs and P2P
work the same way.

### 1. Generate a key pair

```bash
desk keygen
# Public Key:  65160c38…
# Private Key: 9f2d0b17…
```

### 2. Start deskconn-userd in standalone mode on the machine

```bash
desk serve --mode standalone --public-key 65160c38… [--public-key <another public key> ...]
```

deskconn-userd serves QUIC on UDP `0.0.0.0:18080` by default. Pick the transport with `--transport quic|webtransport|both`
and the addresses with `--quic-address` and `--webtransport-address` (default `0.0.0.0:18081`, path `/wamp`). Clients
holding one of the `--public-key`s can connect, whatever authid they present. To run it as the service, put the flags
on `ExecStart` (as `desk serve --mode standalone ...`) with `systemctl --user edit deskconn-userd`.

Both transports use TLS. Pass your certificate with `--tls-cert cert.pem --tls-key key.pem` (e.g. one issued by a public
CA); without them deskconn-userd generates a self-signed certificate once and keeps it in `~/.deskconn`. Either way it logs
the certificate's SHA-256 fingerprint at startup:

```
standalone: certificate SHA-256 fingerprint 3f9a1c… (clients that don't trust its issuer pin it with desk profile add --cert-hash 3f9a1c…)
```

To allow username/password logins instead of (or as well as) keys, add `--user username:password`, repeated for more
accounts (or set `DESKCONND_USERS`, one `username:password` per line, to keep passwords off the command line):

```bash
desk serve --mode standalone --user alice:s3cret [--user <username:password> ...]
```

#### Or from a config file

`desk serve --config <file>` reads the mode and the standalone settings from a YAML file instead, which keeps keys and
passwords off the command line and the service's `ExecStart`. `desk serve init [<path>]` writes a commented sample
to start from (default `serve.yaml` in the current directory, readable only by you; `--force` to overwrite). Every key is optional
and has the default of its flag:

```yaml
mode: standalone            # user or standalone
standalone:
  transport: quic           # quic, webtransport or both
  quic-address: 0.0.0.0:18080
  webtransport-address: 0.0.0.0:18081
  tls-cert: cert.pem        # relative paths are relative to the config file
  tls-key: key.pem
  public-keys:
    - 65160c38…
  users:
    - username: alice
      password: s3cret
    - username: bob
      password: hunter2
```

```bash
chmod 600 serve.yaml   # it holds passwords; desk serve warns if others can read it
desk serve --config serve.yaml
```

Flags given as well override the file, and `--public-key`/`--user` add to its keys and users. To run it as the
service, use `ExecStart=… serve --config <file>` with `systemctl --user edit deskconn-userd`.

### 3. Save it as a profile on the client

```bash
desk profile add office \
  --url quic://203.0.113.5:18080 \
  --cert-hash 3f9a1c… \
  --private-key 9f2d0b17…
```

Then pick it with `--profile`. Commands take no machine argument, since the profile is the machine:

```bash
desk --profile office shell
desk --profile office shell -- uname -a
desk --profile office file cp ./notes.txt :/home/me/notes.txt   # remote paths start with ':'
desk --profile office file cat :/etc/hostname
desk --profile office port forward 8080:80
```

For a username/password account, save the profile without `--private-key`, with the username as `--authid` (default:
the current user). desk asks for the password each time it connects; it's never saved:

```bash
desk profile add office --url quic://203.0.113.5:18080 --cert-hash 3f9a1c… --authid alice
desk --profile office shell
# Password for alice:
```

Use `https://host:18081` as the URL to connect over WebTransport instead. `--cert-hash` pins the machine's certificate,
which is what makes a self-signed one trusted; leave it out when the certificate is issued by a CA the client trusts.

Profiles are kept in the `profiles` section of `~/.deskconn/config.yml`, readable only by you. `desk profile ls` lists them and
`desk profile remove <name>` deletes one. `machine ping`, `machine connect`, `machine disconnect` and `ls` work on
machines of your account only.

## Self-hosting

Run your own cloud (router, account service, web app and database) locally with
[deskconn-docker](https://github.com/xconnio/deskconn-docker), then build `deskconn` pointed at it.

### 1. Start the stack

```bash
git clone https://github.com/xconnio/deskconn-docker
cd deskconn-docker
make setup
make run
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
docker compose logs -f account-service   # in deskconn-docker
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

## CLI reference

Run `desk` or `desk --help` for a compact overview of daily commands and management groups.
Use `desk <command> --help` for command details (for example, `desk file --help`), or
`desk --help-long` for all commands and flags, including standalone authentication options.

### Account

```
desk account login  [--username]
desk account logout
desk account whoami
```

### Machines

```
desk machine ls     [--refresh] [--detailed]
desk machine info   <machine> [--mode direct|routed]
desk machine attach [--name] [--username]
desk machine detach
desk machine alias  <machine> <name>
desk machine alias  <machine> --unset
desk machine logs   <machine> [<source>] [-f] [-n N] [--since DURATION] [--mode direct|routed]
desk machine ping   <machine> [--count N]
desk machine connect    <machine>
desk machine disconnect [<machine>] [--all]
```

`desk ls` is a shortcut for `desk machine ls`.

Standalone machines (see [Standalone mode](#standalone-mode-no-cloud)):

```
desk keygen                # key pair: public for desk serve --public-key, private for profile add --private-key
desk profile add    <name> --url <quic://host:port|https://host:port> [--cert-hash <sha256>] [--private-key <hex>] [--authid <user>]
desk profile remove <name>
desk profile ls
desk --profile <name> <command>
```

### Shell

```
desk shell <machine> [--mode direct|routed] [-A] [<command>...]
```

With a command, `desk shell` runs it on the machine and exits; without one it opens an interactive shell.

`-A`/`--agent-forward` forwards your local `ssh-agent` to the shell, like `ssh -A`, so tools run there (`git`,
`ssh`, ...) can authenticate with your local keys without copying them to the machine. As with `ssh -A`, only use it
against machines you trust — anyone with access to the remote shell for the session's duration can ask the forwarded
agent to sign on your behalf.

### File operations

All file commands accept `machine:path` for remote paths and a bare `/path` for local paths.

```
desk file ls  <machine:path> [--mode direct|routed]
desk file mv  <src> <dst>   [--mode direct|routed]
desk file cp  <src> <dst>   [-r] [--mode direct|routed]
desk file rm  <target>      [--mode direct|routed]
desk file cat <machine:path> [--mode direct|routed]
```

### Port forwarding

```
# Both commands use listen:target (TCP ports 1–65535).
# Listen locally on 127.0.0.1:8080; connect to localhost:80 on office.
desk port forward office 8080:80 [--mode direct|routed]

# Listen on port 9000 on all interfaces on office; connect to local localhost:3000.
desk port reverse office 9000:3000 [--mode direct|routed]
```

### Self-update

```
desk self version
desk self update
```

## Adding an app

Apps beyond deskconn-userd's own (Files, Terminal, Resource Monitor, Text Editor) run as their own processes, like
[media-app](https://github.com/xconnio/media-app) and [remotecontrol-app](https://github.com/xconnio/remotecontrol-app).
An app needs no code in deskconn: it connects to deskconn-userd's local socket, registers its procedures there, and
tells deskconn-userd which apps it serves. deskconn-userd lists them to clients (the web app shows an app only while
it's listed and enabled), owners and admins enable or disable them, and xlink exposes their procedures to the cloud
and P2P.

1. **Connect** to `~/.deskconn/deskconn.sock` (WAMP over a Unix socket, realm `io.xconn.deskconn.local`, anonymous).
   Reconnect when deskconn-userd restarts.
2. **Register a key exchange.** deskconn only supports end-to-end encrypted apps: deskconn-userd refuses an app without one.
   Clients call it with their X25519 public key and get the app's; both sides derive their keys with HKDF-SHA256,
   `frontendToBackend` for requests and `backendToFrontend` for results. Every other procedure takes its request as a
   single encrypted JSON argument (nonce + ChaCha20-Poly1305 ciphertext, empty or absent when there is none) and returns
   a single encrypted JSON result. Keys are per caller session.
3. **Register the apps** by calling `io.xconn.deskconn.local.apps.register` with a JSON list:

   ```json
   [{"id": "pictures", "name": "Pictures", "key_exchange": "io.xconn.deskconn.deskconnd.media.key.exchange",
     "requires": ["files"], "default_off": true}]
   ```

   `requires` are apps this one can't work without (it's disabled with them), and a `default_off` app stays disabled
   until an owner or admin enables it. Ids must be unique on the machine. The apps are listed until the app's session
   leaves.
4. **Follow enabled apps.** Call `io.xconn.deskconn.deskconnd.capabilities.list` for every app's
   `{"id", "name", "enabled", "key_exchange", "requires"}`, and subscribe to
   `io.xconn.deskconn.deskconnd.capabilities.on_change`, published with the same list whenever an app is enabled or
   disabled. Register an app's procedures while it's enabled and unregister them while it's disabled; keep the key
   exchange registered.

Procedures under `io.xconn.deskconn.deskconnd.` are reachable by remote clients (and `io.xconn.deskconn.admin.` by
the machine's owner and admins only), so the key exchange and the app's procedures go there. Everything else on the
socket, such as `io.xconn.deskconn.local.*`, stays local.

In Go, `common.AppEncryption` does step 2:

```go
enc := common.NewAppEncryption()
if err := enc.Register(session, "io.xconn.deskconn.deskconnd.myapp.key.exchange"); err != nil {
    return err
}
handler := enc.Encrypted(func(ctx context.Context, req []byte) (any, error) {
    // req is the decrypted JSON request; the result is sent back as encrypted JSON.
    return map[string]string{"hello": "world"}, nil
})
resp := session.Register("io.xconn.deskconn.deskconnd.myapp.hello", handler).Do()
```

A client finds the app's key exchange in the apps list, then calls it before the app's procedures:

```go
keys, err := common.ClientKeyExchangeWith(session, app.KeyExchange)
result, err := common.EncryptedCall(session, "io.xconn.deskconn.deskconnd.myapp.hello", nil, keys)
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
| `principals.json`       | Local CryptoSign principals (used by the local WAMP router)             |
| `turn_credentials.json` | Cached TURN server credentials for WebRTC                               |
| `deskconn.sock`         | Unix socket for local CLI–daemon communication                          |

## License

See [LICENSE](LICENSE).
