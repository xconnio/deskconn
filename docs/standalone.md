# Standalone mode (no cloud)

`desk serve --mode standalone` serves a machine directly, without an account or the Hub, to the users you
list: with a password, or with a key from `desk keygen`. Shell, files, ports, agents, logs and direct
connections work as usual.

On the machine, write a config, add your users to it, and serve it. It logs the certificate's fingerprint for clients
to pin:

```bash
desk serve init                   # writes ./deskconn.yaml, with every setting explained
desk serve --config deskconn.yaml
```

On the client, add the machine by name. Nothing connects yet, so the machine can be offline:

```bash
desk keygen                       # a key pair: the public key goes in the user's authorized-keys on the machine
desk machine add office --url quic://203.0.113.5:18080 --username alice \
  --cert-hash <fingerprint> --private-key <private-key>
```

Then use it by name, like a machine of your account. Signed in or not, `desk ls` lists it with the mode
`standalone`:

```bash
desk shell office
desk machine ping office
desk file cp ./notes.txt office:/tmp/notes.txt
```

`--username` defaults to your user name on the client. Leave out `--private-key` to be asked for the user's
password instead, which `desk machine add` saves in `~/.deskconn/config.yml` (readable only by you) beside any
private key, so connecting never asks. Leave out `--cert-hash` to verify the machine's certificate against the
system's trusted CAs instead of pinning it. `desk machine remove office` removes the machine from the client again.

While the local daemon (`desk serve`) runs, commands reach a standalone machine through its persistent connection,
as they do a machine of your account: `desk machine connect office` connects ahead of time, `desk ls` shows since
when, and the daemon reconnects if the machine goes away, signed in or not. Without the daemon, each command
connects on its own.

A standalone machine's name can't be a name, alias or the start of an ID of one of your account's machines. If one
of those gets the same name later, the CLI refuses to guess and `desk ls` warns about it: reach the managed machine
by its full ID, or remove the standalone machine and add it again under another name.

`desk serve run --help` and `desk machine add --help` cover the rest, such as serving over WebTransport
(`--url https://host:port`), using your own TLS certificate, and passing users as flags instead of a config file.

Users can also be added and removed while the machine is served, by its admins: the config file's users with
`role: admin`. They do it through the `com.deskconn.admin.users.list`, `.add` and `.remove` procedures. The
users added are members, kept in `~/.deskconn/standalone-users.db` beside the config file's users, which only the
config file changes.

The procedures take plain arguments, so any WAMP client can call them. For clients that only speak WebSocket,
standalone mode also serves `ws://127.0.0.1:18082/ws`: plain, without TLS, so only on localhost.

