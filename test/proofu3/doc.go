// Package proofu3 holds a proof-only, test-only reproduction harness for
// hypothesis U3 (linux/amd64 cross-session wait4 isolation). It contains no
// production code and nothing here is imported by the server, hub, or debugger.
//
// The actual harness is behind the `proofu3` build tag so it never compiles
// into the default `go build ./...` / `go test ./...` runs. See
// proof_u3_linux_amd64_test.go.
package proofu3
