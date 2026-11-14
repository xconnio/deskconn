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

