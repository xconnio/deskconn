GO := go
export CGO_ENABLED := 0

DESKCONN_MODULE := github.com/xconnio/deskconn/cmd

define go-build
	$(GO) build $(DESKCONN_MODULE)/$1
endef

define go-run
	$(GO) run $(DESKCONN_MODULE)/$1
endef

.PHONY: all
all: build-deskconnd build-deskconn

.PHONY: build-deskconnd
build-deskconnd:
	$(call go-build,deskconnd)

.PHONY: run-deskconnd
run-deskconnd:
	$(call go-run,deskconnd)

.PHONY: build-deskconn
build-deskconn:
	$(call go-build,deskconn)

.PHONY: run-deskconn
run-deskconn:
	$(call go-run,deskconn)

.PHONY: test
test:
	$(GO) test -count=1 -v ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: release-snapshot
release-snapshot:
	goreleaser release --snapshot --clean

.PHONY: release-check
release-check:
	goreleaser check

.PHONY: build-deskconn-vpnd
build-deskconn-vpnd:
	$(call go-build,deskconn-vpnd)

.PHONY: run-deskconn-vpnd
run-deskconn-vpnd:
	$(call go-run,deskconn-vpnd)

.PHONY: install
ifeq ($(OS),Windows_NT)
install:
	powershell -ExecutionPolicy Bypass -File install.ps1
else
install:
	sh install.sh
endif
