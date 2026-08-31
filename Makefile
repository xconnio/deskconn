# Router the binaries connect to by default, e.g. `make build CLOUD_QUIC_ADDRESS=127.0.0.1:8081`
CLOUD_QUIC_ADDRESS ?=
LDFLAGS := $(if $(CLOUD_QUIC_ADDRESS),-X github.com/xconnio/deskconn/common.cloudQUICAddress=$(CLOUD_QUIC_ADDRESS))

# export (rather than "CGO_ENABLED=0 go build ...") sets this on the recipe's environment
# directly from make itself, so it works whichever shell runs the recipe - sh/bash here, but
# cmd.exe on Windows when make can't find a sh.exe on PATH (e.g. invoked from PowerShell),
# which doesn't understand that inline-assignment syntax.
export CGO_ENABLED := 0

# 'desk' is reserved by Windows for Display Settings (desk.cpl) and can't be safely
# overridden (see install.ps1), so there's no 'desk' alias built on Windows - use the
# 'deskconn' binary itself there.
ifeq ($(OS),Windows_NT)
DESKCONN_BIN := bin/deskconn.exe
USERD_BIN    := bin/deskconn-userd.exe
ROOTD_BIN    := bin/deskconn-rootd.exe
else
DESKCONN_BIN := bin/deskconn
USERD_BIN    := bin/deskconn-userd
ROOTD_BIN    := bin/deskconn-rootd
endif

test:
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

# One multicall binary: run as desk (or deskconn) it's the CLI, as deskconn-userd the user
# daemon and as deskconn-rootd the root helper. Built once per name (rather than symlinked)
# so this works under whichever shell make ends up using, including cmd.exe on Windows,
# which has neither unprivileged symlinks nor an "ln"/"cp" builtin.
build:
	go build -ldflags "$(LDFLAGS)" -o $(DESKCONN_BIN) github.com/xconnio/deskconn/cmd/deskconn
	go build -ldflags "$(LDFLAGS)" -o $(USERD_BIN) github.com/xconnio/deskconn/cmd/deskconn
	go build -ldflags "$(LDFLAGS)" -o $(ROOTD_BIN) github.com/xconnio/deskconn/cmd/deskconn
ifneq ($(OS),Windows_NT)
	go build -ldflags "$(LDFLAGS)" -o bin/desk github.com/xconnio/deskconn/cmd/deskconn
endif

run-userd:
	go run github.com/xconnio/deskconn/cmd/deskconn deskconn-userd

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
