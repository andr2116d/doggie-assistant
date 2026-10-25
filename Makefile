BIN     := bin/doggie
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build run test lint fmt tidy clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/doggie

run: build
	./$(BIN) run

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "gofmt: files above need formatting"; exit 1)
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	rm -rf bin
