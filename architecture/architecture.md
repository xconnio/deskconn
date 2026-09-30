## Account Creation

```mermaid
flowchart TD

User[User]
App[Website / Mobile App]
AccountService[Account Service]
CloudDB[(Cloud Database)]

User -->|Create Account| App
App -->|API Call| AccountService
AccountService -->|Store User| CloudDB
AccountService -->|Response| App
```

## Organization Creation

```mermaid
flowchart TD

User[User]
App[Website / Mobile App]
AccountService[Account Service]
CloudDB[(Cloud Database)]

User -->|Create Organization| App
App -->|Create Org API| AccountService
AccountService -->|Store Organization| CloudDB
AccountService -->|Response| App
```

## Desktop Attach

```mermaid
flowchart TD

User[User]
DeskconnCLI[deskconn-cli]
AccountService[Account Service]
CloudDB[(Cloud Database)]
CloudRouter[cloud-router]
Deskconnd[deskconnd]

User -->|Run attach command| DeskconnCLI

DeskconnCLI -->|Send Credentials| AccountService
AccountService -->|Validate User| CloudDB

DeskconnCLI -->|Provide / Check Organization| AccountService
AccountService -->|Create Org if needed| CloudDB

DeskconnCLI -->|Attach Desktop| AccountService
AccountService -->|Store Desktop Info| CloudDB

AccountService -->|Start REALM| CloudRouter

CloudRouter -->|Notify Desktop| Deskconnd

Deskconnd -->|Register Procedures| CloudRouter
```

## Persistent connection

```mermaid
sequenceDiagram
    participant C as deskconn-cli
    participant D as deskconnd
    participant Dev as Device

    C->>D: Call procedure (targetDeviceID)

    D->>D: Check if session exists for targetDeviceID

    alt Session exists
        D->>D: Reuse existing session
    else Session does not exist
        D->>Dev: Create new session
        Dev-->>D: Session established
        D->>D: Store session for future use
    end

    D->>Dev: Forward procedure call using session
    Dev-->>D: Response
    D-->>C: Return response
```

### Raw streams

Shell, exec, logs, port forwarding, agent forwarding and file transfer don't use procedure calls: they
run on raw streams opened alongside the session (QUIC streams, or WebRTC data channels once the
connection has upgraded to P2P). By default the CLI opens those on the same persistent connection,
through `deskconnd`'s stream proxy socket (`~/.deskconn/deskconn-streams.sock`), instead of connecting
to the device itself. `--mode quic` and `--mode p2p` still connect directly, as does the default mode
when `deskconnd` isn't running.

```mermaid
sequenceDiagram
    participant C as deskconn-cli
    participant D as deskconnd
    participant Dev as Device

    C->>D: Which transport? (targetDeviceID)
    D->>D: Reuse or create the session, as above
    D-->>C: quic | webrtc

    C->>D: Open stream (targetDeviceID, transport, label)
    D->>Dev: Open QUIC stream / data channel on the session's connection
    D-->>C: OK

    C->>Dev: Key exchange, then encrypted traffic (deskconnd only relays it)
```

The two transports speak different protocols per feature, so `deskconnd` can't translate between them:
the CLI asks which one the connection has and uses that one for the whole operation. The key exchange
is end to end between the CLI and the device, so `deskconnd` relays ciphertext only.
