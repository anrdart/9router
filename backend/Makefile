# 9Router Go backend Makefile. All targets are thin wrappers around go so the
# same commands work whether invoked from backend/ or via npm scripts.

GO ?= go

.PHONY: build run test vet fmt clean

build:
	$(GO) build -o ../bin/9router-backend ./cmd/server

run:
	$(GO) run ./cmd/server

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf ../bin/9router-backend
	$(GO) clean -testcache
