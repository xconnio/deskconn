# DeskConn connectivity adapters

This internal package owns DeskConn’s cloud service contract, credential and
principal files, host identity discovery, realm permissions, and application registration. It was extracted from the XConn Go binding to keep the reusable library
independent of DeskConn policy.

RPC forwarding delegates to XConn’s shared Rust bridge. DeskConn owns its cloud
reconnect policy.
QUIC, WebRTC, WAMP, authentication crypto and remote I/O also remain in Rust.

`StartAppLayer` creates the Unix yamux boundary for local application
processes. WAMP messages and RPC-associated byte streams share that connection.
`App` lists procedures or prefixes and optionally supplies a session
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
WebRTC providers and accepted connection workers.

Configure `Address`, `Realm`, `CredentialsFile`, `PrincipalsFile`,
`ProcedureListKeys`, and the key-add/key-remove/detach topic formats. Existing
`TLSConfig` certificate pools and pinning callbacks are supported; explicit DER
roots may alternatively be supplied through `RootCertificates`. `MachineID`
overrides host discovery, `RetryInterval` changes the initial reconnect delay
(up to 30 seconds), and `OnError` receives reconnect failures. Otherwise failures use Go's logger.
Initialization has a 15-second bound; reconnect delay grows to 30 seconds and
resets after an established session ends. The cloud cycle explicitly owns its two WAMP sessions and bridge because their
combined enrollment and authorization protocol is DeskConn-specific. Cleanup
stops direct providers, closes the shared connection and joins accepted connection workers.
There is no additional public attachment container.

The direct WebRTC provider uses the synchronized principal store. Added keys
can establish new direct sessions; removed keys cannot. Existing authenticated
connections retain their lifetime until closed. Raw channels require a successful
WAMP authentication. Reliable feature streams are positional RPC arguments or
results, forwarded through the same Unix yamux application connection. VPN uses
a native unreliable data channel through `App.DatagramChannel`.

`RunStandalone(ctx, app, config)` serves the device without cloud attachment.
Configure QUIC and/or WebTransport listener addresses, a TLS certificate, the
realm, and authorized public keys or WAMP-CRA users. Both transports use the
same application bridge and authenticated WebRTC provider. Cancellation closes
listeners and sessions and joins accepted connection workers.
`WebTransportPath` is `/wamp`. `NewStandaloneAuthenticator` copies the supplied
credentials so callers cannot mutate an active policy accidentally.

RPC stream references are forwarded by the Rust bridge, including its per-caller
in-process sessions. Local applications connect with `unix+yamux://`. File,
shell, port, agent and log endpoints use the same encrypted byte protocol over
QUIC, WebTransport and native WebRTC RPC streams. WebRTC uses one reliable,
ordered data channel per stream; it does not add yamux over SCTP.
