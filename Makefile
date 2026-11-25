.PHONY: all build test vet fmt verify dist clean

BIN := offerhound
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

all: fmt vet test build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BIN) ./cmd/offerhound

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l $$(find . -name '*.go'))" || (gofmt -l . && echo "run: gofmt -w ." && exit 1)

verify: build
	./bin/$(BIN) verify docs/examples

dist:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist
