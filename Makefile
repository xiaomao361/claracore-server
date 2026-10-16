.PHONY: build run test check clean

BINARY := claracore-server
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) ./cmd/server

run:
	CLARACORE_DEV_USER=$${CLARACORE_DEV_USER:-dev} go run ./cmd/server

test:
	go test ./... -count=1

check:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "gofmt 未通过"; exit 1)
	go vet ./...
	go test ./... -count=1

clean:
	rm -f $(BINARY)
