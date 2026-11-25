# Persistent terminal sessions

A persistent terminal session keeps its shell, and everything running in it, through the client
disconnecting, the network dropping, and the DeskConn daemon restarting, updating or crashing. A
client detaches and attaches again later, from the same or another client of the same user.

`desk shell MACHINE` opens one. Access revocation is yet to build on it.

## Using it

```
desk shell office                              # a new session
desk shell office --list                       # your sessions there
desk shell office --resume ID                  # attach to one
desk shell office --resume ID --takeover       # attach to one attached elsewhere, detaching it there
desk shell office --terminate ID               # end one and its processes
```

- **Ctrl+] then d** detaches: the session runs on, and `desk shell` prints the command to resume it.
  Ctrl+] typed twice sends one Ctrl+]; followed by any other key, it's sent along with that key. Ctrl+C,
  Ctrl+D and `exit` go to the shell as ever.
- **A lost connection is attached again**, to the same session, for up to 5 minutes: `desk shell` says
  it's reconnecting, and the keys typed meanwhile are dropped. It attaches with `since`, so only the
  history it missed is sent, and says so if some of that is gone. A dead connection is noticed when
  its transport gives up on it, which takes QUIC about 30 s.
- **Every `desk shell MACHINE` creates a session**; only `--resume` attaches to one, and it never creates
  one. Another attachment taking over ends `desk shell` with status 1; the shell exiting, with its status.
- Leaving the session, `desk shell` returns the local terminal to its normal screen and turns off the
  modes the session's programs set (mouse reporting, bracketed paste, keypad modes and the like), as it
  tracks them from the output.
- `desk shell MACHINE COMMAND` is unchanged: the command runs without a session, and ends with its
  connection. So does `desk shell MACHINE` with input that isn't a terminal (a pipe, a file), and, with
  a warning, on a machine without persistent sessions (an older DeskConn).

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
  so a command writing output while nobody watches never blocks. It keeps the session's screen and
  history from it (see [Restoring the screen](#restoring-the-screen)).
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

## Restoring the screen

The worker runs the session's output through a terminal emulator of its own (`internal/terminal/screen.go`,
on the `charmbracelet/x/ansi` parser). It keeps what a terminal showing the session would show: the normal
and the alternate screen, the cursor, the scroll region, the tab stops, the window title, and the modes
that change what input means (application cursor keys and keypad, bracketed paste, mouse reporting, focus
events, `modifyOtherKeys`, the kitty keyboard protocol) or how the screen looks. Modes are tracked as the
program sets them; nothing depends on which program it is. The parser keeps its state between reads, so a
sequence or a character split across reads is put together, and nothing is matched as a substring.

- **History** is the lines that scroll off the top of the normal screen, kept as the text and colors to
  write them again, with the rows a long line was wrapped into joined, so a client wraps it at its own
  width. The alternate screen keeps none: full-screen programs redraw it, and their redraws don't pile
  up. The normal screen and its history are kept while the alternate one is in use.
- **Budgets.** Each session keeps up to 16 MiB of history, and all of a daemon's sessions up to 256 MiB:
  `desk serve --terminal-history` and `--terminal-history-total`. Once the total runs low, a new session
  gets what's left, down to 1 MiB; then none starts (`limit`). When a session's history is full, the
  oldest lines go. The budget is the history's own: the screens (at most 1000 × 500 cells each), the
  parser (4 KiB of a sequence's data) and an attachment's queue (8 MiB beyond its snapshot) are bounded
  apart from it.
- **Attaching** sends a snapshot first: written to a terminal, it clears its screen, writes the history
  and the normal screen as lines, so the history lands in the terminal's own scrollback, then the
  alternate screen if it's in use, the modes and the cursor. The snapshot is taken at one point of the
  output, with the attachment made current there, so the output that follows it misses and repeats
  nothing. Reconnecting with `since`, the output offset it has seen, a client is sent only the history
  that scrolled off after it, besides the screen. `history_gap` tells it history it would have been sent
  is gone.
- **Sizes.** Attaching at another size resizes the screen first, then the terminal: the snapshot is at
  the new size, and the programs redraw for it. Lines aren't reflowed but cut or padded; a shorter screen
  scrolls its top lines into the history to keep the cursor's line in view.
- **A slow attachment** is sent its output through a queue of its own. One that falls 8 MiB behind is
  dropped rather than slowing the session down; attaching again restores the screen.

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
| `create` | `cols`, `rows`, `dir` | `session`, `token`, `offset`, `snapshot`, `history_gap`; then attached |
| `attach` | `session`, `cols`, `rows`, `takeover`, `token`, `since` | as for `create` |
| `list` | | `sessions`: `id`, `command`, `created`, `attached` |
| `terminate` | `session` | `session` |

While attached:

- client → machine: input data, `size` (`cols`, `rows`), `ping` (keeps an idle stream alive), `detach`.
- machine → client: output data, then `exit` (`exit_code`; the session is gone) or `displaced` (another
  attachment took over; the session runs on). The output starts with the `snapshot` bytes restoring the
  screen as it was at output offset `offset`; the session's output from there follows. A client adds the
  length of the output after the snapshot to `offset` for the offset it has seen, to attach again with
  as `since`.
- The stream closing for any other reason, on either side, only detaches.

Refusal codes: `not_found` (no such session, ended, or not the caller's), `attached` (attached
elsewhere, no takeover), `unavailable` (the worker can't be reached), `disabled` (the terminal app is
disabled), `limit` (the sessions running have all the history the machine allows), `invalid`, `failed`.

The worker protocol between the daemon and a worker has a version, as a worker can outlive the daemon
that started it. Version 2 added the snapshot; a version 1 worker's attachment has none (`snapshot` 0).

`attach` never creates a session. Input sent while a connection is breaking isn't replayed: whatever
didn't reach the machine is lost, so a command is never run twice.

## Validation

The tests run real workers, shells and, for the client, `desk shell`'s attachment, against a daemon
in the test process; restarting the daemon is starting another on the same directory. The
end-to-end scenarios are `internal/userd/terminalscenarios_unix_test.go`; the rest are in
`internal/terminal` and `internal/userd`.

| Scenario | Automated | Notes |
| --- | --- | --- |
| Connection drops during a long command | `TestScenarioOutputAcrossReconnect`, `TestCLIReconnects` | Every line arrives; only the screen redrawn is repeated |
| Keys typed while reconnecting | `TestScenarioInputWhileReconnectingIsDropped` | Dropped, never run later |
| Daemon restarts, updates or is killed during a command | `TestScenarioDaemonRestartWhileCommandRuns`, `TestTerminalSessionSurvivesDaemonRestart` | Kill and update: real-device checklist |
| Over 16 MiB of output while detached | `TestScenarioOutputPastHistoryLimitWhileDetached`, `TestDetachedOutputDoesNotBlock` | Command never blocks; `history_gap` reported |
| Resources reclaimed when sessions end | `TestScenarioEndedSessionsFreeTheirHistory`, `TestShellExitEndsSession` | |
| Vim, htop on the alternate screen | `TestVimRestoresAlternateScreen`, `TestCLICreateDetachResume` | htop: real-device checklist |
| Takeover by the same user | `TestCLITakeover`, `TestTerminalSessionOwnership` | |
| Another user | `TestTerminalSessionOwnership`, `TestOtherOwnersSessionsAreHidden` | Reported as `not_found` |
| Worker failure, machine reboot | `TestScenarioWorkerFailureEndsTheSession`, `TestDeadWorkerIsUnavailable` | No replacement shell |
| Shell exits, session terminated | `TestCLICreateDetachResume`, `TestTerminalSessionExit`, `TestTerminateKillsSessionProcesses` | |
| Routed and direct transports | — | Real-device checklist; an attachment doesn't move between them |
| Client sleeps and resumes | `TestCLIReconnects` (a drop) | Reattached within 5 minutes; after that, `--resume` |
| Browser refresh, mobile app restart | — | Gap: the web and mobile apps don't use persistent sessions yet |
| Access revoked, attached or detached | — | Gap: revocation isn't built (it needs per-key identities) |

### Real-device checklist

On each platform, against a machine running the new daemon (`desk shell MACHINE --list` works),
in `desk shell MACHINE`:

1. Run `for i in $(seq 1000); do echo count-$i; sleep 0.5; done`, detach (Ctrl+] then d), and
   `--resume` a minute later: the count went on, and count-1 is in the terminal's scrollback.
2. With it attached, take the network away for a minute and give it back: `desk shell` says it's
   reconnecting, then shows the same shell (`echo $$` is the same as before), with nothing missed.
3. Restart the daemon (`systemctl --user restart deskconn`; on macOS and Windows, the service's
   equivalent), then kill it (`systemctl --user kill -s SIGKILL --kill-whom=main deskconn`), then
   update it (`desk self update`): each time, the session reconnects to the same shell.
4. Run `htop` or `vim`, detach, resume: it's as it was, and quitting it returns to the shell's screen.
5. From a second terminal, `--resume` fails with `add --takeover`; `--resume ID --takeover`
   attaches, and the first terminal says it was taken over.
6. Repeat 1 with `--mode routed` and `--mode direct`.
7. `exit 3` ends `desk shell` with status 3; `--terminate ID` ends a detached session; `--list` then
   shows neither.
8. Reboot the machine with a session detached: `--list` is empty, and `--resume ID` fails with
   "no session".

## Platform status

| | Linux | macOS | Windows |
| --- | --- | --- | --- |
| Worker survives daemon restart and kill | Validated with a systemd user service: `systemctl kill -s SIGKILL --kill-whom=all`, `systemctl restart`, and a binary update | Not validated: workers lead their own process group, which launchd's job cleanup leaves alone | Not validated: breakaway from the task's job, if the job allows it |
| Terminate reaches the whole session | Validated, including jobs in their own process groups | Process group only: there's no `/proc` to find the rest of the session | Job object (not validated) |
| Automated tests | `internal/terminal`, `internal/userd` (real worker processes, and Vim), in CI | Same tests, in CI | Build only (the screen's tests are platform-independent) |
| Real-device checklist | 1, 4, 5, 7 and the restart in 3, against a standalone daemon; the rest not run | Not run | Not run |

Known gaps:

- **Windows upgrades end sessions.** `install.ps1` stops every `deskconn.exe` before replacing it,
  since Windows locks a running executable, and that includes workers. Keeping them needs the
  installer to stop only the daemon and move the locked executable aside.
- **Agent forwarding** (`desk shell -A`) points `SSH_AUTH_SOCK` at the forwarding of the connection
  that created the session; it doesn't follow later attachments, so `-A` goes with new sessions only.
- **An attachment stays on the connection it started on.** Today's shell moves to a direct (WebRTC)
  connection once there is one; an attachment doesn't, but attaching again uses the daemon's connection
  as it is then.
- Machine reboots, and a worker failing, end the session: attaching then fails with `not_found`, and
  no replacement shell is started.
- **While detached, terminal queries go unanswered.** A program asking the terminal something (its
  cursor position, its colors) gets no answer until a client is attached to give one.
- **What's not restored**: hyperlinks (OSC 8), images (sixel, kitty graphics), the clipboard (OSC 52),
  and lines reflowed to a new width. Characters are laid out with the widths most terminals use; a
  terminal disagreeing about one (some emoji) shows that line shifted until it's redrawn.
- **A client's terminal clearing its screen into its scrollback** (some do, for the clear the snapshot
  starts with) shows that screen twice in its scrollback.
