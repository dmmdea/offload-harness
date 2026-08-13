.PHONY: build test lint install clean

BIN_EXT := $(if $(filter windows,$(shell go env GOOS)),.exe,)

build:
	go build -o bin/llamaswap-pp-cli$(BIN_EXT) ./cmd/llamaswap-pp-cli

test:
	go test ./...

lint:
	golangci-lint run

install:
	go install ./cmd/llamaswap-pp-cli

clean:
	rm -rf bin/

build-mcp:
	go build -o bin/llamaswap-pp-mcp$(BIN_EXT) ./cmd/llamaswap-pp-mcp

install-mcp:
	go install ./cmd/llamaswap-pp-mcp

build-all: build build-mcp
