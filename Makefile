BINARY  := smlog
GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w

.PHONY: all build build-linux-amd64 build-linux-arm64 test run clean install

all: build

## Local build
build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

## Cross-compile: Linux amd64 (static, ready to copy to the test machine)
build-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-amd64 .

## Cross-compile: Linux arm64
build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY)-linux-arm64 .

## Run all unit tests
test:
	$(GO) test -v ./...

## Run locally (-dev reads web/index.html from disk, root ./test_results)
run: build
	./$(BINARY) -root ./test_results -dev

## Install to /usr/local/bin
install: build
	sudo install -m 0755 $(BINARY) /usr/local/bin/$(BINARY)

clean:
	rm -f $(BINARY) $(BINARY)-linux-amd64 $(BINARY)-linux-arm64
