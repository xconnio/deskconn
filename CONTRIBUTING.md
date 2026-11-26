# Contributing

## Packages

DeskConn is one binary: the `desk` CLI, and with `serve` the machine's daemons. Each package
owns one side of that.

| Package                 | Owns                                                                              |
|-------------------------|-----------------------------------------------------------------------------------|
| `cmd/deskconn`          | CLI parsing, user-facing output, and starting the daemons, installs and updates   |
| `cmd/deskconn/top`      | The `desk top` terminal UI                                                        |
| `internal/deskconn`     | Client operations the CLI runs against a machine: shell, files, mounts, ports, VPN, agents, logs, and choosing how to reach the machine |
| `internal/userd`        | The machine side of every feature: the handlers the user daemon serves            |
| `internal/terminal`     | Persistent terminal sessions: the workers that own their shells, and the terminal emulator that restores their screens |
| `internal/agent`        | Finding the Claude Code and Codex sessions on this machine                        |
| `internal/thumbnail`    | Image, video and PDF thumbnails                                                   |
| `internal/rootd`        | Privileged operations: TUN devices, routes, DNS and firewall for the VPN          |
| `internal/vpnhelper`    | The client and protocol of rootd's socket, used by the CLI, the daemon and rootd  |
| `internal/vpnwin`       | What the VPN client needs from Windows: the Wintun device, routes and DNS          |
| `internal/connectivity` | Connection policy and lifecycle: cloud and standalone serving, connecting to machines and to the local daemon, credentials, machine identity, reconnects |
| `internal/common`       | The contract between client and machine: procedure names, stream messages, framing and payload encryption, and the CLI's config |
| `appkit`                | Public SDK for external DeskConn applications                                     |

Generic protocol, transport, routing and bridging belong in XConn, not here. DeskConn and the
DeskConn Hub don't import each other.

### Dependency rules

- `cmd/deskconn` may import anything here. Nothing imports it.
- `internal/common` imports no other package here. `internal/connectivity` imports only
  `internal/common`.
- `internal/deskconn` and `internal/userd` are the two ends of each feature. What both need, such
  as a procedure name or a message type, goes in `internal/common`. `internal/deskconn` never
  imports `internal/userd`; `internal/userd` uses `internal/deskconn` only where the daemon makes
  a client's request to another machine on the CLI's behalf (uploads, in `uploadproxy.go`).
- `internal/terminal` is imported by `internal/userd`, which serves its sessions, and by
  `internal/deskconn`, for the emulator that tracks what a session left set on the local terminal.
- `appkit` only re-exports from internal packages; its exported names are public API, so keep them
  compatible.

### Where code goes

- A helper used by one feature belongs in that feature's package, next to its only callers.
  `internal/common` is for what both ends of a connection have to agree on; don't add utilities
  to it.
- Keep command handlers in `cmd/deskconn` short: parse flags, call a function in
  `internal/deskconn`, print the result. To reach a machine, use
  `deskconn.ConnectMachineSession` with the mode `deskconn.DefaultMode` resolves rather than
  connecting in the command. Logic that can be tested without a terminal belongs in the package
  it serves.
- Files of a feature share a prefix (`shell*.go`, `file*.go`, `mount*.go`, `portstream*.go`,
  `iptunnel*.go`, `agent*.go`, `terminal*.go`) and platform variants use build suffixes
  (`_linux.go`, `_darwin.go`, `_windows.go`, `_unix.go`).
- Procedure names, roles, permissions, config paths and wire formats are shared with machines,
  apps and the Hub running other versions. Moving code must not change them.
- `cloudQUICAddress` is set at build time with
  `-ldflags "-X github.com/xconnio/deskconn/internal/common.cloudQUICAddress=host:port"`: keep it
  in `internal/common`.

## Building and testing

The XConn native SDK is needed once:

```sh
make native-sdk
```

Then:

```sh
make build                      # bin/deskconn and bin/desk
make test                       # go test -count=1 ./... -v
make lint                       # golangci-lint
go test ./internal/userd -run TestBeginShellSession   # one package or test
```

Tests live next to the code they cover, as `_test.go` files in the same directory. Before sending
a change that touches platform-specific files, check the other platforms build:

```sh
GOOS=windows go vet ./...
GOOS=darwin go vet ./...
```

Keep moves of code and changes to it in separate commits, so a move can be reviewed as one.
