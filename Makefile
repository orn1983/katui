GO ?= go
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

.PHONY: build install

build:
	mkdir -p bin
	$(GO) build -o bin/katui .

install: build
	install -d "$(DESTDIR)$(BINDIR)"
	install -m 0755 bin/katui "$(DESTDIR)$(BINDIR)/katui"
