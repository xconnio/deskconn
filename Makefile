# Router the binaries connect to by default, e.g. `make build CLOUD_QUIC_ADDRESS=127.0.0.1:8081`
CLOUD_QUIC_ADDRESS ?=
LDFLAGS := $(if $(CLOUD_QUIC_ADDRESS),-X github.com/xconnio/deskconn/common.cloudQUICAddress=$(CLOUD_QUIC_ADDRESS))

test:
	go test -count=1 ./... -v

lint:
	golangci-lint run

release-snapshot:
	goreleaser release --snapshot --clean

release-check:
	goreleaser check

build: build-xlink build-deskconnd build-desk

build-xlink:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/xlink github.com/xconnio/deskconn/cmd/xlink

run-xlink:
	go run github.com/xconnio/deskconn/cmd/xlink

build-deskconnd:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/deskconnd github.com/xconnio/deskconn/cmd/deskconnd

run-deskconnd:
	go run github.com/xconnio/deskconn/cmd/deskconnd

build-desk:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/desk github.com/xconnio/deskconn/cmd/desk

run-desk:
	go run github.com/xconnio/deskconn/cmd/desk
