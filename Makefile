test:
	go test -count=1 ./... -v

lint:
	golangci-lint run

release-snapshot:
	goreleaser release --snapshot --clean

release-check:
	goreleaser check

build-deskconnd:
	CGO_ENABLED=0 go build github.com/xconnio/deskconn/cmd/deskconnd

run-deskconnd:
	go run github.com/xconnio/deskconn/cmd/deskconnd

build-deskconn:
	CGO_ENABLED=0 go build github.com/xconnio/deskconn/cmd/deskconn

run-deskconn:
	go run github.com/xconnio/deskconn/cmd/deskconn
