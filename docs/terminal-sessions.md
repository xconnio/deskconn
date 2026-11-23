# Persistent terminal sessions

A persistent terminal session keeps its shell, and everything running in it, through the client
disconnecting, the network dropping, and the DeskConn daemon restarting, updating or crashing. A
client detaches and attaches again later, from the same or another client of the same user.

This is the machine side. The client controls (`desk shell --resume` and the like), restoring the
screen on attach, and access revocation build on it.

## How it works

Each session runs in a **terminal worker**, a process of its own: the daemon's executable, run as the
hidden `deskconn terminal-worker` command. The worker owns the session's PTY, its shell and the shell's
process group. It serves them on a Unix socket in `~/.deskconn/terminals/`, next to a record of the
session (`<id>.json`: owner, command, start time, worker PID and a secret).

```
client ──(QUIC / WebRTC, end-to-end encrypted)──▶ daemon ──(Unix socket)──▶ terminal worker ──▶ PTY ─▶ shell
```

- **The daemon holds no session state.** It starts workers, finds them from their records, and relays
  a client's attachment to one. A restarted or updated daemon finds the sessions the previous one left
  running, under the same IDs.
- **Workers outlive the daemon.** Under systemd, a worker is started in a scope of its own
  (`systemd-run --scope`, as `deskconn-terminal-<id>.scope`), since stopping the daemon's service kills
  everything in its cgroup. Without systemd, the worker leads a session and process group of its own.
  On Windows it's detached from the daemon's console and, where the job allows it, broken away from
  the scheduled task's job.
- **Output keeps flowing while detached.** The worker reads the PTY whether or not anything is attached,
  so a command writing output while nobody watches never blocks. That output is dropped for now: the
  session's screen and history are restored by separate work. Until then, attaching again redraws by
  resizing the terminal, which full-screen programs and shell line editors answer by redrawing.
  A new session's first output waits up to 10 s for its first attachment, so the creator sees the
  first prompt.
- **Local access is private.** The terminals directory is the user's own, mode 0700; records and
  sockets are 0600. Each connection to a worker also presents the secret from the session's record.
- **Ended sessions aren't kept.** When the shell exits, the worker gives an attached client the exit
  status, kills what's left of the shell's process group, removes its record and socket, and exits.
  A worker that dies leaves a record whose socket refuses connections: the daemon removes it when
  it next comes across it, and at startup (as after a reboot).

### Sessions and owners

A session's ID is 128 random bits. It belongs to the identity that created it: the caller's
authenticated authid, scoped to how the daemon authenticates (an account in user mode, a user in
standalone mode). Every operation checks the owner. Someone else's session, or one that has ended,
is reported the same way, as not found, so an ID alone tells nothing.

One attachment controls a session at a time. Attaching to a session that's attached elsewhere needs
an explicit takeover, or the attachment token of the attachment being replaced (a client reconnecting
after its connection dropped). The displaced attachment is told and its later input is ignored.

### Terminating

Terminating hangs up the shell's process group, as closing a terminal would, and kills the shell and
every process in its session 3 s later (on Linux, found by session ID, which also reaches an
interactive shell's jobs). On Windows, the shell runs in a job object, and terminating ends the job.
Processes that left the session on purpose (`setsid`, daemons) are left alone, as they would be by
closing a terminal.

## Protocol

Clients use the `com.deskconn.terminal.stream` stream procedure, served by the terminal app (disabling
the app removes it). Machines without it answer "no such procedure", so a client never mistakes an old
machine's plain shell for a persistent one. The existing `com.deskconn.shell.stream` is unchanged: it
still serves today's shells and `desk shell MACHINE COMMAND`, which end with their connection.

The stream carries the same end-to-end encrypted envelopes as the shell protocol: control messages are
JSON `TerminalControlMsg` (`internal/common/terminal.go`), data frames are terminal input and output.
The first message is the request; the machine replies with one control message, with `code` and
`error` set if it refused.

| Request | Fields | Reply |
| --- | --- | --- |
| `create` | `cols`, `rows`, `dir` | `session`, `token`; then attached |
| `attach` | `session`, `cols`, `rows`, `takeover`, `token` | `session`, `token`; then attached |
| `list` | | `sessions`: `id`, `command`, `created`, `attached` |
| `terminate` | `session` | `session` |

While attached:

- client → machine: input data, `size` (`cols`, `rows`), `ping` (keeps an idle stream alive), `detach`.
- machine → client: output data, then `exit` (`exit_code`; the session is gone) or `displaced` (another
  attachment took over; the session runs on).
- The stream closing for any other reason, on either side, only detaches.

Refusal codes: `not_found` (no such session, ended, or not the caller's), `attached` (attached
elsewhere, no takeover), `unavailable` (the worker can't be reached), `disabled` (the terminal app is
disabled), `invalid`, `failed`.

`attach` never creates a session. Input sent while a connection is breaking isn't replayed: whatever
didn't reach the machine is lost, so a command is never run twice.

## Platform status

| | Linux | macOS | Windows |
| --- | --- | --- | --- |
| Worker survives daemon restart and kill | Validated with a systemd user service: `systemctl kill -s SIGKILL --kill-whom=all`, `systemctl restart`, and a binary update | Not validated: workers lead their own process group, which launchd's job cleanup leaves alone | Not validated: breakaway from the task's job, if the job allows it |
| Terminate reaches the whole session | Validated, including jobs in their own process groups | Process group only: there's no `/proc` to find the rest of the session | Job object (not validated) |
| Automated tests | `internal/terminal`, `internal/userd` (real worker processes) | Same tests; not run in CI here | Build only |

Known gaps:

- **Windows upgrades end sessions.** `install.ps1` stops every `deskconn.exe` before replacing it,
  since Windows locks a running executable, and that includes workers. Keeping them needs the
  installer to stop only the daemon and move the locked executable aside.
- **Agent forwarding** (`desk shell -A`) points `SSH_AUTH_SOCK` at the forwarding of the connection
  that created the session; it doesn't follow later attachments.
- Machine reboots, and a worker failing, end the session: attaching then fails with `not_found`, and
  no replacement shell is started.
