# Rust-backed XConn development build

DeskConn uses `github.com/xconnio/xconn-core/bindings/go` for WAMP, QUIC,
WebTransport and WebRTC. The former `xconn-go`, `wampproto-go`,
`xconn-webrtc-go`, Pion and quic-go modules are no longer dependencies.
File search streams encrypted progressive RPC results through the shared Rust bridge.
File-cat and file search use encrypted progressive RPC results.
File uploads and downloads use the existing encrypted transfer channels.

This local integration uses a sibling checkout at `../xconn-core` through the
`go.mod` replacement. Replace that development dependency with a published module version before release.

## Build and test on Linux or macOS

Install Go, a C compiler and pkg-config, and build the native SDK once from the
XConn checkout. Rust is needed to build the SDK; consumers of a packaged SDK do
not need Rust.

From the DeskConn checkout:

```sh
make native-sdk
make build
make test
```

Rerun `make native-sdk` after changing Rust code. Make automatically finds the
staged SDK and enables cgo. `XCONN_DIR` defaults to `../xconn-core`;
`XCONN_SDK_DIR` defaults to that checkout's `target/native`. Override either
variable on the make command line when using another checkout or SDK directory.
Existing `PKG_CONFIG_PATH` entries take precedence over the local SDK.
The Go module replacement still points to `../xconn-core/bindings/go`; update it
separately if moving the binding checkout.

For direct Go commands, set `CGO_ENABLED=1` and point `PKG_CONFIG_PATH` to the
SDK's `lib/pkgconfig` directory. Keep the SDK installed at the location used
during linking, or configure the platform loader to find its shared library.
The default build links the shared native library.

## Standalone Linux executable

```sh
make build-static
strip bin/deskconn bin/desk
```

This builds the Rust SDK and statically links both `bin/deskconn` and `bin/desk`.
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
Before publishing this migration, make those jobs install a pinned native SDK,
bundle the matching shared library, and use a published Go module version.
The current native SDK supports Linux and macOS; the old Windows release target
needs native SDK/binding support before it can be restored. Do not use the
existing release configuration to distribute this development integration.

The migration is exercised by the DeskConn race suite, including encrypted file
transfers, standalone QUIC/WebTransport authentication, persistent stream proxies
and Rust-backed WebRTC channels. Independent original-Go interoperability tests
live in the XConn repository.

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
storage, device roles and local relay framing. Generic RPC bridging and reconnect
supervision run in the Rust XConn engine. Packet encryption also delegates to
`bindings/go/crypto`, preserving the existing key labels and packet format.
The WAMP client APIs and native transport paths are unchanged.
