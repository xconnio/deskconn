# Router the binaries connect to by default, e.g. `make build CLOUD_QUIC_ADDRESS=127.0.0.1:8081`
CLOUD_QUIC_ADDRESS ?=
LDFLAGS := $(if $(CLOUD_QUIC_ADDRESS),-X github.com/xconnio/deskconn/internal/common.cloudQUICAddress=$(CLOUD_QUIC_ADDRESS))

# export (rather than "CGO_ENABLED=0 go build ...") sets this on the recipe's environment
# directly from make itself, so it works whichever shell runs the recipe - sh/bash here, but
# cmd.exe on Windows when make can't find a sh.exe on PATH (e.g. invoked from PowerShell),
# which doesn't understand that inline-assignment syntax.
export CGO_ENABLED := 0

# 'desk' is reserved by Windows for Display Settings (desk.cpl) and can't be safely
# overridden (see install.ps1), so there's no 'desk' built on Windows - use the
# 'deskconn' binary itself there.
ifeq ($(OS),Windows_NT)
DESKCONN_BIN := bin/deskconn.exe
GO := go
else
DESKCONN_BIN := bin/deskconn
# Falls back to where check-go.sh installs Go, for the recipes that run right after it does,
# before PATH has it. Expanded when a recipe runs, so after check-go.
GO = $(shell command -v go || echo /usr/local/go/bin/go)
endif

.PHONY: check-go
ifeq ($(OS),Windows_NT)
check-go:
else
check-go:
	@sh check-go.sh
endif

test: check-go
	$(GO) test -count=1 ./... -v

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
# root helper. Outside Windows, desk is a symlink to it, like the installed layout.
build: check-go
	$(GO) build -ldflags "$(LDFLAGS)" -o $(DESKCONN_BIN) github.com/xconnio/deskconn/cmd/deskconn
ifneq ($(OS),Windows_NT)
	ln -sf deskconn bin/desk
endif

run-userd: check-go
	$(GO) run github.com/xconnio/deskconn/cmd/deskconn serve --mode user

run-desk: check-go
	$(GO) run github.com/xconnio/deskconn/cmd/deskconn

.PHONY: install
ifeq ($(OS),Windows_NT)
install:
	powershell -ExecutionPolicy Bypass -File install.ps1
else
install:
	sh install.sh
endif
