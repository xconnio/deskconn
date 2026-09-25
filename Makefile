test:
	go test -count=1 ./... -v

lint:
	golangci-lint run

release-snapshot:
	goreleaser release --snapshot --clean

release-check:
	goreleaser check

build: build-xlink build-deskconnd build-deskconn build-deskconn-vpnd

build-xlink:
	CGO_ENABLED=0 go build -o bin/xlink github.com/xconnio/deskconn/cmd/xlink

run-xlink:
	go run github.com/xconnio/deskconn/cmd/xlink

build-deskconnd:
	CGO_ENABLED=0 go build -o bin/deskconnd github.com/xconnio/deskconn/cmd/deskconnd

run-deskconnd:
	go run github.com/xconnio/deskconn/cmd/deskconnd

build-deskconn:
	CGO_ENABLED=0 go build -o bin/deskconn github.com/xconnio/deskconn/cmd/deskconn

run-deskconn:
	go run github.com/xconnio/deskconn/cmd/deskconn

build-deskconn-vpnd:
	CGO_ENABLED=0 go build -o bin/deskconn-vpnd github.com/xconnio/deskconn/cmd/deskconn-vpnd

run-deskconn-vpnd:
	go run github.com/xconnio/deskconn/cmd/deskconn-vpnd
