# Build the Target, build BinGo and run the Target
default: build-target build run

# Usage: just build 				-> 	bingo_linux_amd64 (Default)
#		 just build darwin arm64    -> 	bingo_darwin_arm64 (MacOs specified, ARM64 specified)
# Build the BinGo binary. Takes positional arguments for the target OS and architecture (Must be valid `go build` targets).
build OS="linux" ARCH="amd64":
	go clean
	mkdir -p ./build/bingo
	{{ if OS == "darwin" { "env CGO_ENABLED=1 GOOS=" + OS + " GOARCH=" + ARCH + " go build -tags bingonative -o ./build/bingo/bingo_" + OS + "_" + ARCH + " ./cmd/bingo && codesign --sign - --entitlements entitlements.plist --force ./build/bingo/bingo_" + OS + "_" + ARCH } else { "env GOOS=" + OS + " GOARCH=" + ARCH + " go build -o ./build/bingo/bingo_" + OS + "_" + ARCH + " ./cmd/bingo" } }}

# Usage: just run 										->	runs ./build/bingo/bingo_linux_amd64 (Default)
#		 just run darwin arm64 							-> 	runs ./build/bingo/bingo_darwin_arm64 (MacOs specified, ARM64 specified)
#		 just run linux amd64 -addr 127.0.0.1:6061 -v 	->  runs ./build/bingo/bingo_linux_amd64 -addr 127.0.0.1:6061 -v

# ARGS:  -addr string    listen address (default ":6060")
#		 -v              verbose logging (debug level)
# Run the BinGo binary. Takes positional arguments for the target OS and architecture (Must be existing binaries).
run OS="linux" ARCH="amd64" *ARGS="":
	./build/bingo/bingo_{{OS}}_{{ARCH}} {{ARGS}}

# Builds then runs the bingo binary for the target OS and architecture (Must be valid `go build` targets).
server OS="linux" ARCH="amd64" *ARGS="": build-target (build OS ARCH) (run OS ARCH ARGS)

# Build the Target with maximum debugging information
build-target:
	mkdir -p ./build/target
	go build --gcflags="all=-N -l" -o ./build/target/target ./cmd/target

# ARGS: -addr string    server address (default "localhost:6060")
#	  	-session string session ID to join (omit to create a new session)
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
