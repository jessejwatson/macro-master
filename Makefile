VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
ifeq ($(VERSION),)
VERSION := dev
endif
PREFIX  ?= $(HOME)/.local
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test install uninstall dist clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/mm ./cmd/mm

test:
	go vet ./...
	go test ./...

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/mm $(PREFIX)/bin/mm

uninstall:
	rm -f $(PREFIX)/bin/mm

# Source tarball in the layout a Homebrew formula expects.
dist:
	rm -rf dist && mkdir -p dist/macro-master-$(VERSION)
	tar --exclude=./dist --exclude=./bin --exclude=./.git -cf - . | tar -xf - -C dist/macro-master-$(VERSION)
	tar -czf dist/macro-master-$(VERSION).tar.gz -C dist macro-master-$(VERSION)
	rm -rf dist/macro-master-$(VERSION)
	@shasum -a 256 dist/macro-master-$(VERSION).tar.gz

clean:
	rm -rf bin dist
