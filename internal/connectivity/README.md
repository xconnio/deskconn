# DeskConn connectivity adapters

This package owns DeskConn's cloud service contract, credential and principal
files, host identity, device roles, and reconnect policy. WAMP, transports,
authentication crypto, RPC forwarding and stream carriers run in XConn's Rust
engine.

`StartAppLayer` serves local applications over yamux. Unix systems use a Unix
socket. Windows uses a loopback listener authenticated with a random secret in
an owner-only endpoint file. `ConnectApp` selects the matching transport.
`App` lists the bridged procedures and prefixes, and optionally creates a local
session with each remote caller's authid and authrole.

`NewBridge` returns an explicitly owned Rust bridge. Close it when its remote
session ends. It forwards arguments, results, errors, cancellation and RPC
streams, and releases per-caller sessions on remote leave events. Per-caller
bridges require caller disclosure and session-leave events on the remote realm.

`Run` waits for credentials, connects the device and cloud realms over QUIC,
exposes application procedures and updates authorized cryptosign principals.
It subscribes before fetching the initial key snapshot, bounds queued updates,
persists snapshots atomically and reconnects after transport loss. Detachment
removes credentials and returns to waiting. Cancellation closes sessions,
bridges, the connection and WebRTC provider. Configure the cloud address,
realm, credential/principal paths and key-list/add/remove/detach endpoints.
`MachineID` overrides host discovery. `RetryInterval` sets the initial backoff;
it grows to 30 seconds and resets after an established connection ends.

`RunStandalone` serves configured QUIC, WebTransport or WebSocket endpoints
without cloud attachment. Configure authentication, TLS and realm settings.
Cancellation closes listeners, providers and sessions and joins connection
workers. `WebTransportPath` is `/wamp`.

Files, shell, ports, agents and logs use encrypted RPC byte streams. WebRTC
uses one reliable ordered data channel per stream, without additional yamux.
VPN retains its native unreliable data channel through `App.DatagramChannel`.
Removed keys block new direct sessions; authenticated sessions remain active
until closed.
