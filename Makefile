test:
	go test -count=1 ./... -v

lint:
	golangci-lint run

build-deskconnd:
	go build github.com/xconnio/deskconn/cmd/deskconnd

run-deskconnd:
	go run github.com/xconnio/deskconn/cmd/deskconnd

build-deskconnctl:
	go build github.com/xconnio/deskconn/cmd/deskconnctl

run-deskconnctl:
	go run github.com/xconnio/deskconn/cmd/deskconnctl
