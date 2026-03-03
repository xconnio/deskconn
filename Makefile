test:
	go test -count=1 ./... -v

lint:
	golangci-lint run

build-deskconnd:
	go build github.com/xconnio/deskconn/cmd/deskconnd

run-deskconnd:
	go run github.com/xconnio/deskconn/cmd/deskconnd

build-deskconn:
	go build github.com/xconnio/deskconn/cmd/deskconn

run-deskconn:
	go run github.com/xconnio/deskconn/cmd/deskconn
