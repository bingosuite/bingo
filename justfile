# Build the Target, build BinGo and run the Target
default: build-target build run

system-info:
	@echo "This is a {{os()}} machine with a {{arch()}} CPU".

# Build the BinGo binary
build OS="linux" ARCH="amd64":
	mkdir -p ./build/bingo
	env GOOS={{OS}} GOARCH={{ARCH}} go build -o ./build/bingo/bingo_{{OS}}_{{ARCH}}{{ if OS == "windows" { ".exe" } else { "" } }} ./cmd/bingo

# Build BinGo for all supported platforms (just example for now, we do not support all of these)
build-all:
    just build linux   amd64
    just build linux   arm64
    just build darwin  amd64
    just build darwin  arm64
    just build windows amd64
    just build windows arm64

# Run the BinGo binary with TARGET (defaults to "target")
run OS="linux" ARCH="amd64":
	./build/bingo/bingo_{{OS}}_{{ARCH}}{{ if OS == "windows" { ".exe" } else { "" } }}

go OS="linux" ARCH="amd64": build-target (build OS ARCH) (run OS ARCH)

# Run test cli with optional server and session
# Usage: just cli                    (both default)
#        just cli localhost:8080     (custom server)
#        just cli - abc123           (default server, custom session)
#        just cli localhost:8080 abc123 (both custom)
# Use "-" for default value
cli server="" session="":
	#!/usr/bin/env bash
	set -euo pipefail
	ARGS=""
	if [ -n "{{server}}" ] && [ "{{server}}" != "-" ]; then ARGS="$ARGS --server {{server}}"; fi
	if [ -n "{{session}}" ] && [ "{{session}}" != "-" ]; then ARGS="$ARGS --session {{session}}"; fi
	go run ./cmd/bingo-client/main.go $ARGS

# Build the Target with maximum debugging information
build-target: 
	mkdir -p ./build/target
	go build --gcflags="all=-N -l" -o ./build/target/target ./cmd/target

# Run the target by itself
run-target: 
	./build/target/target

# Build and run the target by itself
go-target: build-target run-target 

# Run unit tests on the PKG (defaults to ./...)
test PKG="./...":
	go test -v {{PKG}}

# Run coverage on the PKG (defaults to ./...)
coverage PKG="./...":
	go test -coverprofile=test/coverage.out {{PKG}}
	go tool cover -func=test/coverage.out

# Run integration tests
integration:
	go run github.com/onsi/ginkgo/v2/ginkgo -r ./test/integration/.