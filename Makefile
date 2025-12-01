test:
	go test -count=1 ./... -v

lint:
	golangci-lint run

build:
	go build github.com/xconnio/deskconn/cmd/desconnd

run:
	go run github.com/xconnio/deskconn/cmd/desconnd
