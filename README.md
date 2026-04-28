# Deskconn

Deskconn is a desktop management suite.
It consists of two main tools: a desktop daemon and a command-line client.

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
  --[no-]help  Show context-sensitive help (also try --help-long and
               --help-man).

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

file pull [<flags>] <device> <remote-path> <local-path>
    Download a file or directory from a device

    -r, --[no-]recursive  Download directories recursively
        --[no-]p2p        Connect using WebRTC

file push [<flags>] <device> <local-path> <remote-path>
    Upload a file or directory to a device

    -r, --[no-]recursive  Upload directories recursively
        --[no-]p2p        Connect using WebRTC

shell [<flags>] <device>
    Start interactive shell

    --[no-]p2p  Connect using WebRTC

exec [<flags>] <device> <command>...
    Run a command

    --[no-]p2p  Connect using WebRTC

printer enable [<flags>]
    Enable receiving print jobs on this desktop

    --[no-]host-printers  Also allow remote clients to list this desktop's
                          printers

printer disable
    Disable receiving print jobs on this desktop


printer status
    Show whether this desktop accepts print jobs


printer list [<flags>] <device>
    List printers on a device

    --[no-]p2p  Connect using WebRTC

printer print --printer=PRINTER [<flags>] <device> <file_path>
    Print a local file on a device

    --printer=PRINTER  Printer name
    --[no-]p2p         Connect using WebRTC

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
