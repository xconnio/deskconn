# Router the binaries connect to by default, e.g. `make build CLOUD_QUIC_ADDRESS=127.0.0.1:8081`
CLOUD_QUIC_ADDRESS ?=
LDFLAGS := $(if $(CLOUD_QUIC_ADDRESS),-X github.com/xconnio/deskconn/common.cloudQUICAddress=$(CLOUD_QUIC_ADDRESS))

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
# daemon and as deskconn-rootd the root helper.
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/deskconn github.com/xconnio/deskconn/cmd/deskconn
	ln -sf deskconn bin/desk
	ln -sf deskconn bin/deskconn-userd
	ln -sf deskconn bin/deskconn-rootd

run-userd:
	go run github.com/xconnio/deskconn/cmd/deskconn deskconn-userd

run-desk:
	go run github.com/xconnio/deskconn/cmd/deskconn
