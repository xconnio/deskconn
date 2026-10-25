# Router the binaries connect to by default, e.g. `make build CLOUD_QUIC_ADDRESS=127.0.0.1:8081`
CLOUD_QUIC_ADDRESS ?=
LDFLAGS := $(if $(CLOUD_QUIC_ADDRESS),-X github.com/xconnio/deskconn/internal/common.cloudQUICAddress=$(CLOUD_QUIC_ADDRESS))

# WAMP and network transports are linked from the native XConn SDK.
XCONN_DIR ?= ../xconn-core
XCONN_SDK_DIR ?= $(XCONN_DIR)/target/native
export PKG_CONFIG_PATH := $(if $(PKG_CONFIG_PATH),$(PKG_CONFIG_PATH):)$(abspath $(XCONN_SDK_DIR))/lib/pkgconfig
export CGO_ENABLED := 1

.PHONY: native-check native-sdk
native-check:
	@pkg-config --exists xconn || (echo "Run make native-sdk or install the XConn native SDK; see docs/native-xconn.md"; exit 1)

# Build once, and rerun after changing Rust transport or protocol code.
native-sdk:
	cargo build --locked --release --manifest-path "$(XCONN_DIR)/Cargo.toml" -p xconn-ffi
	python3 "$(XCONN_DIR)/scripts/package-native.py" --stage-only "$(XCONN_SDK_DIR)"

# 'desk' is reserved by Windows for Display Settings (desk.cpl) and can't be safely
# overridden (see install.ps1), so there's no 'desk' built on Windows - use the
# 'deskconn' binary itself there.
BIN_DIR ?= bin

ifeq ($(OS),Windows_NT)
DESKCONN_BIN := $(BIN_DIR)/deskconn.exe
else
DESKCONN_BIN := $(BIN_DIR)/deskconn
endif

test: native-check
	go test -count=1 ./... -v

.PHONY: lint format

lint:
	golangci-lint run

format:
	golangci-lint fmt
	golangci-lint run --fix

release-snapshot:
	goreleaser release --snapshot --clean

release-check:
	goreleaser check

# One binary: the CLI, and with "serve --mode user|standalone|root" the user daemon or the
# root helper. desk is built as a copy (rather than symlinked) so this works under whichever
# shell make ends up using, including cmd.exe on Windows.
build: native-check
	go build -ldflags "$(LDFLAGS)" -o $(DESKCONN_BIN) github.com/xconnio/deskconn/cmd/deskconn
ifneq ($(OS),Windows_NT)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/desk github.com/xconnio/deskconn/cmd/deskconn
endif

run-userd:
	go run github.com/xconnio/deskconn/cmd/deskconn serve --mode user

run-desk:
	go run github.com/xconnio/deskconn/cmd/deskconn

.PHONY: install
ifeq ($(OS),Windows_NT)
install:
	powershell -ExecutionPolicy Bypass -File install.ps1
else
install:
	sh install.sh
endif

# Fully static Linux executable; needs the host's static libc libraries.
.PHONY: native-sdk-static build-static
native-sdk-static:
	@test "$$(uname -s)" = Linux || (echo "Static builds currently support Linux only"; exit 1)
	cargo build --locked --profile native --manifest-path "$(XCONN_DIR)/Cargo.toml" -p xconn-ffi
	python3 "$(XCONN_DIR)/scripts/package-native.py" --stage-only --static --profile native "$(XCONN_SDK_DIR)"

build-static: native-sdk-static
	go build -a -tags xconn_static,netgo,osusergo -ldflags "$(LDFLAGS) -linkmode external -extldflags '-static -Wl,--gc-sections'" -o $(DESKCONN_BIN) github.com/xconnio/deskconn/cmd/deskconn
	go build -a -tags xconn_static,netgo,osusergo -ldflags "$(LDFLAGS) -linkmode external -extldflags '-static -Wl,--gc-sections'" -o $(BIN_DIR)/desk github.com/xconnio/deskconn/cmd/deskconn
