# Build the Target, build BinGo and run the Target
default: build-target build run

# Writes out the OS and architecture of the system you are currently on.
system-info:
	@echo "This is a {{os()}} machine with a {{arch()}} CPU".

# Usage: just build 				-> 	bingo_linux_amd64 (Default)
#		 just build windows 		-> 	bingo_windows_amd64.exe (Windows specified, default architecture)
#		 just build darwin arm64 -> 	bingo_darwin_arm64 (MacOs specified, ARM64 specified)
# Build the BinGo binary. Takes positional arguments for the target OS and architecture (Must be valid `go build` targets).
build OS="linux" ARCH="amd64":
	mkdir -p ./build/bingo
	env GOOS={{OS}} GOARCH={{ARCH}} go build -o ./build/bingo/bingo_{{OS}}_{{ARCH}}{{ if OS == "windows" { ".exe" } else { "" } }} ./cmd/bingo

# Usage: just run 				->	runs ./build/bingo/bingo_linux_amd64 (Default)
#		 just run windows 		->	runs ./build/bingo/bingo_windows_amd64.exe (Windows specified, default architecture)
#		 just run darwin arm64 	-> 	runs ./build/bingo/bingo_darwin_arm64 (MacOs specified, ARM64 specified)
# Run the BinGo binary. Takes positional arguments for the target OS and architecture (Must be existing binaries).
run OS="linux" ARCH="amd64":
	./build/bingo/bingo_{{OS}}_{{ARCH}}{{ if OS == "windows" { ".exe" } else { "" } }}

# Builds then runs the bingo binary for the target OS and architecture (Must be valid `go build` targets).
go OS="linux" ARCH="amd64": build-target (build OS ARCH) (run OS ARCH)

# Build BinGo for all supported platforms (just example for now, we do not support all of these)
build-all:
    just build linux   amd64
    just build linux   arm64
    just build darwin  amd64
    just build darwin  arm64
    just build windows amd64
    just build windows arm64

# Usage: just cli                    (both default)
#        just cli localhost:8080     (custom server)
#        just cli - abc123           (default server, custom session)
#        just cli localhost:8080 abc123 (both custom)
# Use "-" for default value
# Run test cli with optional server and session
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