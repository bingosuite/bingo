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
	{{ if OS == "darwin" { "env CGO_ENABLED=1 GOOS=" + OS + " GOARCH=" + ARCH + " go build -tags bingonative -o ./build/bingo/bingo_" + OS + "_" + ARCH + " ./cmd/bingo && codesign --sign - --entitlements entitlements.plist --force ./build/bingo/bingo_" + OS + "_" + ARCH } else { "env GOOS=" + OS + " GOARCH=" + ARCH + " go build -o ./build/bingo/bingo_" + OS + "_" + ARCH + (if OS == "windows" { ".exe" } else { "" }) + " ./cmd/bingo" } }}

# Usage: just run 				->	runs ./build/bingo/bingo_linux_amd64 (Default)
#		 just run windows 		->	runs ./build/bingo/bingo_windows_amd64.exe (Windows specified, default architecture)
#		 just run darwin arm64 	-> 	runs ./build/bingo/bingo_darwin_arm64 (MacOs specified, ARM64 specified)
# Run the BinGo binary. Takes positional arguments for the target OS and architecture (Must be existing binaries).
run OS="linux" ARCH="amd64":
	./build/bingo/bingo_{{OS}}_{{ARCH}}{{ if OS == "windows" { ".exe" } else { "" } }}

# Builds then runs the bingo binary for the target OS and architecture (Must be valid `go build` targets).
go OS="linux" ARCH="amd64": build-target (build OS ARCH) (run OS ARCH)

# Build BinGo for all supported platforms
build-all:
    just build linux   amd64

# Build the Target with maximum debugging information
build-target:
	mkdir -p ./build/target
	go build --gcflags="all=-N -l" -o ./build/target/target ./cmd/target

# Run the target by itself
run-target:
	./build/target/target

# Build and run the target by itself
go-target: build-target run-target

# Run the bingo debug server (native platform, auto-detected).
# Builds with cgo + bingonative tag, codesigns with debugger entitlement on macOS.
server *ARGS:
	go build -tags bingonative -o ./build/bingo/bingo_server ./cmd/bingo
	{{ if os() == "macos" { "codesign --sign - --entitlements entitlements.plist --force ./build/bingo/bingo_server" } else { "" } }}
	./build/bingo/bingo_server {{ARGS}}

# Build and run the interactive CLI client
cli *ARGS:
	go run ./cmd/cli {{ARGS}}

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
