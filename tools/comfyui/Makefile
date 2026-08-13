.PHONY: build test lint install clean

BIN_EXT := $(if $(filter windows,$(shell go env GOOS)),.exe,)

build:
	go build -o bin/comfyui-pp-cli$(BIN_EXT) ./cmd/comfyui-pp-cli

# -race     catches data races the functional assertions cannot see.
# -shuffle  randomises test order and PRINTS ITS SEED, so an order-dependent
#           failure is both discoverable and reproducible (-shuffle=<seed>).
# -count=1  defeats the test cache, so `make test` always actually runs.
test:
	go test ./... -count=1 -race -shuffle=on

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
