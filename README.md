# Deskconn

A split operating system where the runtime lives on your computer, the interface lives on any device, and applications
can execute locally or in the cloud.

## Installation
```bash
curl -fsSL https://get.deskconn.com | sh
```

## deskconnd

The desktop daemon responsible for registering and exposing desktop APIs to the Deskconn ecosystem. It runs in the
background and enables remote management and control of the desktop.

### Run

```bash
make run-deskconnd
```

## deskconn

A command-line tool used to manage desktops. It can attach a desktop to the cloud, execute commands, transfer files,
manage printers, and perform other administrative tasks.

### Build

```bash
make build-deskconn
```

### Run

Run the resulting binary to see available commands and help:

```bash
./deskconn
```

### Supported commands

Command line interface looks like this:

```text
usage: deskconn <command> [<args> ...]

Deskconn control CLI

Flags:
  --[no-]help  Show context-sensitive help (also try --help-long and --help-man).

Commands:
help [<command>...]
    Show help.


attach [<flags>]
    Attach a device

    -n, --name=NAME            Device name
    -u, --username=USERNAME    Username
    -p, --password=PASSWORD    Password
        --[no-]password-stdin  Read password from stdin

detach [<flags>]
    Detach device

    -u, --username=USERNAME    Username
    -p, --password=PASSWORD    Password
        --[no-]password-stdin  Read password from stdin

login [<flags>]
    Login and store credentials

    -u, --username=USERNAME    Username
    -p, --password=PASSWORD    Password
        --[no-]password-stdin  Read password from stdin

file ls [<flags>] <target>
    List files on a device

    --mode=MODE  Connection mode: 'p2p' uses direct WebRTC, 'routed' uses router, default auto-migrates from routed to p2p

file mv [<flags>] <src> <dst>
    Move or rename a file or directory on a device

    --mode=MODE  Connection mode: 'p2p' uses direct WebRTC, 'routed' uses router, default auto-migrates from routed to p2p

file cp [<flags>] <src> <dst>
    Copy files to/from/between devices

    -r, --[no-]recursive  Copy directories recursively
        --mode=MODE       Connection mode: 'p2p' uses direct WebRTC, 'routed' uses router, default auto-migrates from routed to p2p

file rm [<flags>] <target>
    Remove a file or directory on a device

    --mode=MODE  Connection mode: 'p2p' uses direct WebRTC, 'routed' uses router, default auto-migrates from routed to p2p

shell [<flags>] <device>
    Start interactive shell

    --mode=MODE  Connection mode: 'p2p' uses direct WebRTC, 'routed' uses router, default auto-migrates from routed to p2p

exec [<flags>] <device> <command>...
    Run a command

    --[no-]p2p  Connect using WebRTC

print [<flags>] [<target>] [<file_path>]
    Print operations

    --[no-]enable         Enable receiving print jobs on this desktop
    --[no-]host-printers  Also allow remote clients to list this desktop's printers (use with --enable)
    --[no-]disable        Disable receiving print jobs on this desktop
    --[no-]status         Show whether this desktop accepts print jobs
    --ls=LS               List printers on a device (device name or alias)
    --[no-]p2p            Connect using WebRTC

port forward [<flags>] <device> [<ports>]
    Forward a local port to a port on the remote device

    -l, --local=LOCAL    Local port to listen on
    -r, --remote=REMOTE  Port on the remote device to connect to
        --[no-]p2p       Connect using WebRTC

ls [<flags>]
    List devices

    --[no-]refresh   Refresh device list from cloud
    --[no-]detailed  Show detailed output

whoami
    Show current user


logout
    Logout


config show
    Show config


config set <device> <key> <value>
    Set device alias


config unset <device> <key>
    Unset device alias


config edit
    Edit full config


self version
    Show the installed deskconn version


self update
    Check for updates and install the latest release
```
