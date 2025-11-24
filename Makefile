test:
	go test -count=1 ./... -v

lint:
	golangci-lint run

build:
	go build github.com/xconnio/deskconnd/cmd/desconnd

run:
	go run github.com/xconnio/deskconnd/cmd/desconnd
