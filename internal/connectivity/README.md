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
removes credentials and returns to waiting. It advertises the device for
direct connections with XConn's `Expose` (`io.xconn.peer.*` signaling), and keeps the
`com.deskconn.webrtc.*` provider for older clients, direct mode and VPN; a Hub that doesn't
allow the new signaling yet only leaves clients on the routed path. The device's own sessions
are `RoutedOnly`. Cancellation closes sessions,
bridges, the connection and WebRTC provider. Configure the cloud address,
realm, credential/principal paths and key-list/add/remove/detach endpoints.
`MachineID` overrides host discovery. `RetryInterval` sets the initial backoff;
it grows to 30 seconds and resets after an established connection ends.

`RunStandalone` serves configured QUIC, WebTransport or WebSocket endpoints
without cloud attachment. Configure authentication, TLS and realm settings.
Cancellation closes listeners, providers and sessions and joins connection
workers. `WebTransportPath` is `/wamp`.

The client side connects to machines: `ConnectMachineRealmQUIC` reaches a machine
through the cloud, or a standalone machine directly with `ConnectStandalone`, and keeps every
call there; `ConnectMachineRealmManaged`, which the daemon uses, has XConn send each new call
over a direct path once the machine has one, and over the cloud otherwise;
`ConnectWebrtcSession` negotiates a direct WebRTC session; `ConnectDaemon` joins
the current user's local daemon.

Files, shell, ports, agents and logs use encrypted RPC byte streams. WebRTC
uses one reliable ordered data channel per stream, without additional yamux.
VPN retains its native unreliable data channel through `App.DatagramChannel`.
Removed keys block new direct sessions; authenticated sessions remain active
until closed.
