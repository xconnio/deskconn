# Building with XConn Core

DeskConn uses `github.com/xconnio/xconn-core/bindings/go` for WAMP, QUIC,
WebTransport and WebRTC. The former `xconn-go`, `wampproto-go`,
`xconn-webrtc-go`, Pion and quic-go modules are no longer dependencies.
File-cat and file search use encrypted progressive RPC results.
File uploads, downloads, shell, ports, agent forwarding and logs use encrypted
RPC-associated byte streams. VPN retains its native datagram channel.

The app uses the published Core Go module and Native SDK `v0.1.6`. A sibling
Core checkout and Rust are not required.

## Build and test on Linux or Apple Silicon macOS

Install Go, a C compiler and pkg-config. Install the released native SDK once,
then use the normal build commands:

```sh
make native-sdk
make build
make test
```

The installer downloads and verifies the static SDK and configures standard
pkg-config discovery. It may request sudo for installation. `make build` links
XConn into the executable, so no separate XConn shared library is needed at
runtime. System libraries and certificate stores remain requirements.

An app build does not reinstall the SDK. After switching SDK versions, force a
relink with `go build -a` or use `make build-static`. For an SDK extracted to a
custom location, pass `XCONN_SDK_DIR=/path/to/sdk` to Make. Direct Go commands can
use `PKG_CONFIG_PATH=/path/to/sdk/lib/pkgconfig` and `CGO_ENABLED=1`.

## Standalone Linux executable

```sh
make build-static
strip bin/deskconn bin/desk
```

This uses the installed SDK and produces `bin/deskconn`, with `bin/desk` as a copy.
No `libxconn_ffi.so` or OpenSSL shared library is needed to run them. Install the
host C toolchain, pkg-config, and static development libraries for libc. TLS uses
rustls/ring, so OpenSSL development packages are no longer needed.
The Rust native profile enables thin LTO, one codegen unit, and size optimization.
Panic unwinding is retained. The linker discards unused sections, and the build
forces Go to rebuild so cgo cannot reuse stale SDK paths or native archives.
The executable targets the build host architecture; this is not a cross-compilation
target. System certificate files are still needed for certificate verification.

Use `BIN_DIR=/path/to/output` to choose another output directory. The static
build uses the `xconn_static`, `netgo`, and `osusergo` Go tags and external static
linking. Linux glibc runtime facilities may still load system modules; this does
not promise a hermetic executable on every Linux distribution.

## Releases and Windows

Releases build Linux amd64/arm64 and Windows amd64 using the matching published
static SDKs. GoReleaser enables Cgo and selects GCC, ARM64 GCC or MinGW-w64 GCC.
`ci/prepare-release-sdk.sh` downloads and verifies each archive. The release
workflow installs these toolchains before building. Windows arm64 is excluded
because XConn does not support it yet.

Prebuilt macOS releases are disabled for now. macOS source support is retained;
the published native SDK supports source builds on Apple Silicon.

For a local Windows build, use an MSYS2 MinGW64 shell with Go, GCC and pkg-config:

```sh
ci/prepare-release-sdk.sh /tmp/xconn-sdk windows_amd64
export PKG_CONFIG_PATH=/tmp/xconn-sdk/windows_amd64/lib/pkgconfig
go build -tags xconn_static -ldflags "-linkmode external -extldflags '-static'" -o deskconn.exe ./cmd/deskconn
```

Windows local applications use authenticated loopback yamux because the native
SDK has no Windows Unix-socket support. The daemon writes an owner-only endpoint
file with a fresh random secret. CLI commands use `common.ConnectDaemon`;
applications use `appkit.Connect(ctx, cfgDirectory)` on every platform.
Unix applications may continue using `appkit.SocketURI` with a normal client.

## Shared connectivity

RPC-associated streams, Rust bridge forwarding, Unix yamux application sessions
and native WebRTC stream carriers use the published XConn 0.1.6 binding and SDK.
No XConn source workspace is needed.

The former raw-stream relay sockets and feature data-channel endpoints are
removed. Web and mobile clients must adopt the RPC stream endpoints before
deploying this backend; older raw-channel clients cannot use them.

## Application ownership

`internal/connectivity` owns DeskConn’s attachment protocol, identity/credential
storage and device roles. RPC streams share the local yamux application connection.
Generic RPC bridging runs in the Rust XConn engine; DeskConn owns its cloud
reconnect policy. Packet encryption also delegates to
`bindings/go/crypto`, preserving the existing key labels and packet format.
The WAMP client APIs and native transport paths are unchanged.
