# Building with XConn Core

DeskConn uses `github.com/xconnio/xconn-core/bindings/go` for WAMP, QUIC,
WebTransport and WebRTC. The former `xconn-go`, `wampproto-go`,
`xconn-webrtc-go`, Pion and quic-go modules are no longer dependencies.
File-cat and file search use encrypted progressive RPC results.
File uploads, downloads, shell, ports, agent forwarding and logs use encrypted
RPC-associated byte streams. VPN retains its native datagram channel.

The app uses the published Core Go module and Native SDK `v0.1.6`. A sibling
Core checkout and Rust are not required.

## Build and test on Linux or macOS

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

## Release work still required

The existing CI and GoReleaser setup targets the former standalone Go build.
Before publishing this migration, make those jobs install the released static SDK.
The Go module dependency now resolves from its published release.
The native SDK supports Linux, macOS ARM64 and Windows amd64. Windows builds
require MinGW-w64 GCC and manual SDK extraction; the shell installer supports
Linux and macOS. The release jobs still need per-platform SDK setup. Do not use the
existing release configuration to distribute this development integration.

The migration is exercised by the DeskConn race suite, including encrypted file
transfers, standalone QUIC/WebTransport authentication, persistent stream proxies
and Rust-backed WebRTC channels. Independent original-Go interoperability tests
live in the XConn repository.

## Shared connectivity

RPC-associated streams, Rust bridge forwarding, Unix yamux application sessions
and native WebRTC stream carriers use the published XConn 0.1.6 binding and SDK.
No XConn source workspace is needed.

The former raw-stream relay sockets and feature data-channel endpoints are
removed. Web and mobile clients must adopt the RPC stream endpoints before
deploying this backend; older raw-channel clients cannot use them.

## Static binary size measurements

Measured on Ubuntu 26.04, x86-64, with Go 1.26, retaining every transport.
All sizes are after `strip`, in decimal MB; each row includes preceding changes.

| Build | Size |
| --- | ---: |
| Original static build | 48.60 MB |
| Discard unused linker sections | 41.43 MB |
| Thin LTO and one Rust codegen unit | 37.91 MB |
| Rust size optimization (`opt-level = "z"`) | 34.45 MB |
| Use rustls instead of OpenSSL for WebSocket TLS | 27.88 MB |

The final build is 42.6% smaller. Panic unwinding remains enabled. Functional
Rust, Go binding, DeskConn, and TLS certificate validation tests passed; these
measurements do not establish the throughput impact of size optimization.
QUIC and WebTransport still use separate upstream QUIC implementations.

## Application ownership

`internal/connectivity` owns DeskConn’s attachment protocol, identity/credential
storage and device roles. RPC streams share the Unix yamux application socket.
Generic RPC bridging runs in the Rust XConn engine; DeskConn owns its cloud
reconnect policy. Packet encryption also delegates to
`bindings/go/crypto`, preserving the existing key labels and packet format.
The WAMP client APIs and native transport paths are unchanged.
