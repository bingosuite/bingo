.PHONY: build run go build-target run-target go-target integration

TFN ?= target

build: 
	go build -o ./bin/ ./cmd/BinGo

run:
	./bin/BinGo ${TFN}

go: build-target build run

build-target:
	go build --gcflags="all=-N -l" -o ./bin/ ./cmd/${TFN}

run-target:
	./bin/${TFN}

go-target: build-target run-target
	
integration:
	go run github.com/onsi/ginkgo/v2/ginkgo -r ./test/integration/.