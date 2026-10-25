BIN     := bin/doggie
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
# Where make install puts doggie: ~/.local/bin by default, which needs no
# root. E.g. make install PREFIX=/usr/local.
PREFIX  ?= $(HOME)/.local
BINDIR  ?= $(PREFIX)/bin

.PHONY: build install uninstall run test lint fmt tidy clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/doggie

# A rename, not a copy over the old file: a DOGGIE may be running from it.
install: build
	@mkdir -p "$(BINDIR)"
	@cp $(BIN) "$(BINDIR)/.doggie.new"
	@chmod 0755 "$(BINDIR)/.doggie.new"
	@mv -f "$(BINDIR)/.doggie.new" "$(BINDIR)/doggie"
	@echo "installed $(BINDIR)/doggie"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) echo "note: $(BINDIR) is not in your PATH; add it to your shell profile:"; echo "  export PATH=\"$(BINDIR):\$$PATH\"";; esac

uninstall:
	rm -f "$(BINDIR)/doggie"

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
