# Auto-detected host OS/ARCH, normalized to Go's GOOS/GOARCH naming
# (just's os()/arch() report "macos"/"aarch64"/"x86_64"; Go wants
# "darwin"/"arm64"/"amd64"). Used as the default for build/run/server so
# `just build` (no args) targets the machine it's running on; explicit
# positional args still override.
os_name := if os() == "macos" { "darwin" } else { os() }
arch_name := if arch() == "aarch64" { "arm64" } else if arch() == "x86_64" { "amd64" } else { arch() }

# Build the Target, build BinGo and run the Target
default: build-target build run

# Usage: just build 				-> 	bingo_<host os>_<host arch> (auto-detected)
#		 just build darwin arm64    -> 	bingo_darwin_arm64 (MacOs specified, ARM64 specified)
# Build the BinGo binary. Takes positional arguments for the target OS and architecture (Must be valid `go build` targets).
build OS=os_name ARCH=arch_name:
	go clean
	mkdir -p ./build/bingo
	{{ if OS == "darwin" { "env CGO_ENABLED=1 GOOS=" + OS + " GOARCH=" + ARCH + " go build -tags bingonative -o ./build/bingo/bingo_" + OS + "_" + ARCH + " ./cmd/bingo && codesign --sign - --entitlements entitlements.plist --force ./build/bingo/bingo_" + OS + "_" + ARCH } else { "env GOOS=" + OS + " GOARCH=" + ARCH + " go build -o ./build/bingo/bingo_" + OS + "_" + ARCH + " ./cmd/bingo" } }}

# Usage: just run 										->	runs ./build/bingo/bingo_<host os>_<host arch> (auto-detected)
#		 just run darwin arm64 							-> 	runs ./build/bingo/bingo_darwin_arm64 (MacOs specified, ARM64 specified)
#		 just run linux amd64 -addr 127.0.0.1:6061 -v 	->  runs ./build/bingo/bingo_linux_amd64 -addr 127.0.0.1:6061 -v

# ARGS:  -addr string    listen address (default ":6060")
#		 -v              verbose logging (debug level)
# Run the BinGo binary. Takes positional arguments for the target OS and architecture (Must be existing binaries).
run OS=os_name ARCH=arch_name *ARGS="":
	./build/bingo/bingo_{{OS}}_{{ARCH}} {{ARGS}}

# Builds then runs the bingo binary for the target OS and architecture (Must be valid `go build` targets).
server OS=os_name ARCH=arch_name *ARGS="": build-target (build OS ARCH) (run OS ARCH ARGS)

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

# Run the debugger E2E acceptance tests on linux/amd64 (native ptrace backend).
# Runs every label (no filter): `basic` correctness, `churn` robustness, `pause`
# async-interrupt, `stepping` (StepInto/StepOut), `inspect` (StackFrames/Locals/
# Goroutines), `breakpoints` (ClearBreakpoint), `kill` (kill-while-running),
# `restart`, and `fullstack` transport, all under -race.
e2e-linux:
	go test -tags e2e -race -count=1 -v -timeout 600s ./test/integration

# Run the debugger E2E acceptance tests on darwin/arm64 (native pure-Mach
# exception-port backend). Runs every label (no filter): `basic`, `stepping`,
# `breakpoints`, `churn`, `kill`, `pause`, `inspect`, `restart`, and `fullstack`,
# matching linux. The step-off-an-armed-trap specs and kill-while-running, once
# linux-only under the old wait4 model, are reliable on darwin under the
# Mach-exception rearchitecture (#92) — per-thread exception delivery, a
# target-side I-cache flush on breakpoint writes, and a wait4-free kill (see the
# darwin container and AGENTS.md). task_for_pid needs the debugger entitlement, so
# the test binary is codesigned before it runs.
e2e-darwin:
	mkdir -p ./build
	env CGO_ENABLED=1 go test -tags 'e2e bingonative' -race -c -o ./build/bingo-e2e.test ./test/integration
	codesign --sign - --entitlements entitlements.plist --force ./build/bingo-e2e.test
	./build/bingo-e2e.test -test.v -test.timeout 600s
