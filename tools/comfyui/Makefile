.PHONY: build test lint install clean

BIN_EXT := $(if $(filter windows,$(shell go env GOOS)),.exe,)

build:
	go build -o bin/comfyui-pp-cli$(BIN_EXT) ./cmd/comfyui-pp-cli

test:
	go test ./...

lint:
	golangci-lint run

install:
	go install ./cmd/comfyui-pp-cli

clean:
	rm -rf bin/

build-mcp:
	go build -o bin/comfyui-pp-mcp$(BIN_EXT) ./cmd/comfyui-pp-mcp

install-mcp:
	go install ./cmd/comfyui-pp-mcp

build-all: build build-mcp
