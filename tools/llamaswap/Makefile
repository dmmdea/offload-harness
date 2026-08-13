.PHONY: build test lint install clean

BIN_EXT := $(if $(filter windows,$(shell go env GOOS)),.exe,)

build:
	go build -o bin/llamaswap-pp-cli$(BIN_EXT) ./cmd/llamaswap-pp-cli

# -count=1   defeats the build cache: a cached PASS is not a test run, and this
#            suite asserts against live files (V:\models, the llama-swap backup
#            corpus) whose state changes between runs.
# -shuffle=on randomizes test order and PRINTS ITS SEED, so an order-dependent
#            failure is reproducible with -shuffle=<seed>. Shared package-level
#            state (the emission slot in agent_errors.go, the schema goldens)
#            only shows up as a bug under a shuffled order.
# -race      needs cgo and a C toolchain, so the target enables cgo explicitly;
#            -race silently cannot work without it. On Windows install a MinGW-w64
#            gcc and put it on PATH.
GOTESTFLAGS ?= -race -shuffle=on -count=1

test:
	CGO_ENABLED=1 go test $(GOTESTFLAGS) ./...

# test-fast drops -race for the inner loop. It is NOT the gate: a change is
# green when `make test` is green.
test-fast:
	go test -shuffle=on -count=1 ./...

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
