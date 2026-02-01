.PHONY: build run go build-target run-target go-target test coverage integration

# Extract package path from arguments (if provided)
ARGS := $(filter-out test coverage,$(MAKECMDGOALS))
PKG := $(if $(ARGS),./$(ARGS),./...)

build: 
	go build -o ./build/bingo/bingo ./cmd/bingo

run:
	./build/bingo/bingo target

go: build-target build run

build-target:
	go build --gcflags="all=-N -l" -o ./build/target/target ./cmd/target

run-target:
	./build/target/target

go-target: build-target run-target

test: # make test internal/ws
	go test -v $(PKG)

coverage: # make coverage internal/ws
	go test -coverprofile=test/coverage.out $(PKG)
	go tool cover -func=test/coverage.out

# Prevent make from treating package paths as targets
%:
	@:
	
integration:
	go run github.com/onsi/ginkgo/v2/ginkgo -r ./test/integration/.