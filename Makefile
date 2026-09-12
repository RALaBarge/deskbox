# DeskBox build + release.
#
# Everything here produces static, CGO-free binaries. That is the whole
# portability story: the SQLite driver is pure Go (modernc.org/sqlite), so
# there is no libc to match, no shared object to be missing, and a binary
# built here runs on any Linux of the same architecture — musl or glibc,
# old distro or new.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
PLATFORMS ?= linux/amd64 linux/arm64

GO ?= go
BIN ?= bin
DIST ?= dist

.PHONY: all build shim test check clean dist install

all: build

## build: both binaries for this machine
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/agent-desk ./cmd/agent-desk
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/tcs-shim ./cmd/tcs-shim

## test: vet + the race detector, which is what the concurrency tests need
test:
	$(GO) vet ./...
	$(GO) test -race ./...

## check: ask the built desk whether this box has what the tools need
check: build
	$(BIN)/agent-desk -check

## install: put both binaries somewhere the sandbox can see. /usr/local/bin
## is under /usr, which is bound read-only into every job — a home directory
## is not, and a tcs-shim installed there is invisible to every shimmed tool.
PREFIX ?= /usr/local
install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 $(BIN)/agent-desk $(DESTDIR)$(PREFIX)/bin/agent-desk
	install -m 0755 $(BIN)/tcs-shim $(DESTDIR)$(PREFIX)/bin/tcs-shim

## dist: release tarballs, one per platform, plus checksums
dist: clean
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  stage=$(DIST)/deskbox-$(VERSION)-$$os-$$arch; \
	  echo "  $$os/$$arch"; \
	  mkdir -p $$stage; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
	    -o $$stage/agent-desk ./cmd/agent-desk || exit 1; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
	    -o $$stage/tcs-shim ./cmd/tcs-shim || exit 1; \
	  cp README.md LICENSE.md known.md $$stage/; \
	  cp -r examples $$stage/; \
	  tar -C $(DIST) -czf $$stage.tar.gz $$(basename $$stage); \
	  rm -rf $$stage; \
	done
	@cd $(DIST) && sha256sum *.tar.gz > SHA256SUMS
	@echo "$(DIST)/:" && ls -1 $(DIST)

clean:
	rm -rf $(BIN) $(DIST)
