# Auto-detected host OS/ARCH, normalized to Go's GOOS/GOARCH naming
# (just's os()/arch() report "macos"/"aarch64"/"x86_64"; Go wants
# "darwin"/"arm64"/"amd64"). Used as the default for build/run/server so
# `just build` (no args) targets the machine it's running on; explicit
# positional args still override.
os_name := if os() == "macos" { "darwin" } else { os() }
arch_name := if arch() == "aarch64" { "arm64" } else if arch() == "x86_64" { "amd64" } else { arch() }
vscode_target := if os_name + "/" + arch_name == "darwin/arm64" { "darwin-arm64" } else if os_name + "/" + arch_name == "linux/amd64" { "linux-x64" } else { "unsupported" }

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
#		 -idle-timeout duration
#		                 exit after no managed sessions for this duration;
#		                 omitted/0 keeps the server persistent; positive values
#		                 use whole milliseconds (minimum 1ms)
#		 -v              verbose logging (debug level)
# Run the BinGo binary. Takes positional arguments for the target OS and architecture (Must be existing binaries).
run OS=os_name ARCH=arch_name *ARGS="":
	./build/bingo/bingo_{{OS}}_{{ARCH}} {{ARGS}}

# Default WS (-addr) and DAP (-dap-addr) listen addresses for the server recipes.
# Override on the CLI, e.g. `just server darwin arm64 :7070 :4712`.
ws_addr := ":6060"
dap_addr := ":4711"

# Build then run the server with EVERYTHING enabled: both the WebSocket (-addr)
# and DAP (-dap-addr) listeners, using the standard defaults so a DAP driver
# (VS Code / `just dapcli`) and a `go run ./cmd/wsmon` observer can share one
# session out of the box (see docs/ConcurrencyTelemetry.md). Override addresses
# via the ADDR/DAP_ADDR positionals; extra flags (e.g. `-idle-timeout 30s`) pass
# through in ARGS. No idle timeout is supplied by default, so manual servers
# remain persistent.
server OS=os_name ARCH=arch_name ADDR=ws_addr DAP_ADDR=dap_addr *ARGS="": build-target (build OS ARCH)
	./build/bingo/bingo_{{OS}}_{{ARCH}} -addr {{ADDR}} -dap-addr {{DAP_ADDR}} {{ARGS}}

# Build then run the server with ONLY the WebSocket (-addr) listener; DAP stays
# disabled (the binary leaves -dap-addr empty). Override the address via ADDR;
# extra flags (e.g. -v) pass through in ARGS.
server-ws OS=os_name ARCH=arch_name ADDR=ws_addr *ARGS="": build-target (build OS ARCH)
	./build/bingo/bingo_{{OS}}_{{ARCH}} -addr {{ADDR}} {{ARGS}}

# Build the Target with maximum debugging information
build-target:
	mkdir -p ./build/target
	go build --gcflags="all=-N -l" -o ./build/target/target ./cmd/target

# VS Code's pre-launch task rebuilds the telemetry demo without optimization so
# source breakpoints and stepping stay aligned with the checked-out source.
build-spawntree:
	mkdir -p ./build
	go build -gcflags="all=-N -l" -o ./build/spawntree ./examples/spawntree

# Build the native server into the extension-local layout used by both an
# Extension Development Host and the platform-specific VSIX.
vscode-prepare:
	npm --prefix editors/vscode run binary:prepare

# Prepare source-extension and target artifacts for an Extension Development
# Host. Normal target F5 uses the installed VSIX and only rebuilds spawntree.
vscode-dev: build-spawntree vscode-prepare
	npm --prefix editors/vscode run build

# ARGS: -addr string    server address (default "localhost:6060")
#	  	-session string session ID to join (omit to create a new session)
# Build and run the interactive CLI client
cli *ARGS:
	go run ./cmd/cli {{ARGS}}

# ARGS: -addr string    DAP server address (default "localhost:4711")
#	  	-session string existing session ID to join (omit to create on launch)
# Build and run the interactive DAP CLI client. Drives a session over the Debug
# Adapter Protocol (server's -dap-addr listener); any number of dapcli and cli
# clients can join and drive the same session concurrently.
dapcli *ARGS:
	go run ./cmd/dapcli {{ARGS}}

# Reinstall from the lockfile so local checks exercise the same dependency graph
# as CI rather than whatever happens to be present in node_modules.
vscode-check:
	npm --prefix editors/vscode ci --ignore-scripts
	npm --prefix editors/vscode run check

# Keep the install artifact under the repo-level ignored dist directory so
# packaging never dirties the extension source tree.
vscode-package: vscode-check
	mkdir -p ./dist
	npm --prefix editors/vscode run package:reproducible
	npm --prefix editors/vscode run package:verify

# Explicit opt-in keeps packaging/CI from mutating a developer's normal VS Code
# profile while still providing a one-command local install/update path.
vscode-install: vscode-package
	code --install-extension ./dist/bingo-{{vscode_target}}.vsix --force

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
# `exit` (real exit code), `attach` (attach by PID to a running process),
# `restart`, and `fullstack` transport, all under -race.
e2e-linux:
	go test -tags e2e -race -count=1 -v -timeout 600s ./test/integration

# Run the debugger E2E acceptance tests on darwin/arm64 (native pure-Mach
# exception-port backend). Runs every label (no filter): `basic`, `stepping`,
# `breakpoints`, `churn`, `kill`, `exit`, `attach`, `pause`, `inspect`,
# `restart`, `fullstack`, and the darwin-only `hygiene` (Mach port-right leak),
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
