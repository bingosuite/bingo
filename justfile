# Build the BinGo binary
build: 
    go build -o ./build/bingo/bingo ./cmd/bingo

# Run the BinGo binary with TARGET (defaults to "target")
run:
	./build/bingo/bingo

# Build the Target, build BinGo and run the Target
go: build-target build run

# Build the Target with maximum debugging information
build-target: 
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

client:
    go run ./cmd/client/client.go -server localhost:8080 -session test-session