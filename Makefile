.PHONY: build run go build-target run-target go-target integration

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
	
integration:
	go run github.com/onsi/ginkgo/v2/ginkgo -r ./test/integration/.