# Browser and mobile feature interoperability

This local fixture runs real DeskConn file and shell handlers behind a Unix
yamux application connection and an XConn RPC bridge. It exposes loopback-only
QUIC, WebTransport and WebRTC listeners and creates transfer files in a temporary
directory. Closing stdin shuts down the fixture and removes those files.

With the published XConn native SDK installed:

```sh
go build -tags xconn_static -o /tmp/deskconn-client-peer ./tests/client-peer
```

Run the browser consumer tests from `deskconn-web`:

```sh
DESKCONN_STREAM_PEER=/tmp/deskconn-client-peer \
CHROMIUM_BIN=/path/to/chrome npm run test:transports
```

Run the mobile consumer tests from `deskconn-mobile`:

```sh
DESKCONN_STREAM_PEER=/tmp/deskconn-client-peer flutter test
```

The consumers exercise encrypted multi-megabyte uploads/downloads, multiple
shells on one session, forward/reverse TCP tunnels, error handling and cleanup over both routed and WebRTC
connections. Hub's own integration tests separately cover its production
account authentication, permissions and cloud/device bridging.
