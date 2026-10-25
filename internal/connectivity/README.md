# DeskConn connectivity adapters

This internal package owns DeskConn’s cloud service contract, credential and
principal files, host identity discovery, realm permissions, and local relay
framing. It was extracted from the XConn Go binding to keep the reusable library
independent of DeskConn policy.

RPC forwarding and reconnect supervision delegate to XConn’s shared Rust services.
QUIC, WebRTC, WAMP, authentication crypto and remote I/O also remain in Rust.

`StartAppLayer` creates the Unix RawSocket boundary for local application
processes. `App` lists procedures or prefixes and optionally supplies a session
factory accepting the remote caller's authid and authrole.

`NewBridge(ctx, remote, app)` returns an explicitly owned bridge. It preserves
binary arguments, kwargs, error URIs and error payloads, propagates call
cancellation, and closes per-caller local sessions on remote leave events.
`Close` unregisters its procedures and releases its caller sessions. Per-caller
bridges require caller disclosure and session-leave events on the remote realm.
`RegisterBridge` retains the old convenience API, tying cleanup to the remote
session lifetime. Progressive input and results are forwarded by the shared Rust bridge.

`Run(ctx, app, cloudConfig)` waits for the credentials file, establishes device
and cloud WAMP sessions on one QUIC connection, exposes application procedures,
and maintains authorized cryptosign principals. It subscribes before fetching
the initial key snapshot, buffers updates with a fixed limit, persists snapshots
atomically with private file permissions, and reconnects on transport loss.
Detachment or removal of the device realm removes the credentials file and
returns to waiting for attachment. Cancellation stops active sessions, bridges,
WebRTC providers and owned stream-relay workers.

Configure `Address`, `Realm`, `CredentialsFile`, `PrincipalsFile`,
`ProcedureListKeys`, and the key-add/key-remove/detach topic formats. Existing
`TLSConfig` certificate pools and pinning callbacks are supported; explicit DER
roots may alternatively be supplied through `RootCertificates`. `MachineID`
overrides host discovery, `RetryInterval` changes the initial reconnect delay,
and `OnError` receives reconnect failures. Otherwise failures use Go's logger.
Initialization has a 15-second bound; reconnect delay grows to 30 seconds and
resets after an established session ends.

The direct WebRTC provider uses the synchronized principal store. Added keys
can establish new direct sessions; removed keys cannot. Existing authenticated
connections retain their lifetime until closed. Raw channels require a successful
WAMP authentication. QUIC and WebRTC application traffic is forwarded to
`App.StreamSocket` using DeskConn's existing relay header and text/binary framing.

`RunStandalone(ctx, app, config)` serves the device without cloud attachment.
Configure QUIC and/or WebTransport listener addresses, a TLS certificate, the
realm, and authorized public keys or WAMP-CRA users. Both transports use the
same application bridge and authenticated WebRTC provider. Cancellation closes
listeners and sessions and joins accepted connection and stream-relay workers.
`WebTransportPath` is `/wamp`. `NewStandaloneAuthenticator` copies the supplied
credentials so callers cannot mutate an active policy accidentally.

The separate Peer/RPC-stream design is not part of this migration.
