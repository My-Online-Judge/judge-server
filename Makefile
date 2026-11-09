# All Go tooling runs inside Docker: there is no local Go toolchain.
GO_IMAGE  ?= golang:1.24
BUF_IMAGE ?= bufbuild/buf:1.47.2
GO_RUN     = docker run --rm -v "$(CURDIR)":/src -v judge-server-gomod:/go/pkg/mod -w /src -e GOFLAGS=-mod=mod -e GOCACHE=/tmp/gocache $(GO_IMAGE)
VERSION   ?= dev

.PHONY: proto test vet build tidy image sandbox-test smoke

# Regenerate internal/pb from ../oj-common/src/main/proto/judge_server.proto.
proto:
	docker run --rm -v "$(CURDIR)/..":/ws -w /ws/judge-server $(BUF_IMAGE) generate

test:
	$(GO_RUN) go test ./...

vet:
	$(GO_RUN) go vet ./...

tidy:
	$(GO_RUN) go mod tidy

build:
	$(GO_RUN) sh -c 'CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/judge-server ./cmd/judge-server'

# The image that ships (the runtime stage of the Dockerfile): judge-server:$(VERSION).
image:
	docker build --target runtime --build-arg VERSION=$(VERSION) -t judge-server:$(VERSION) .

# Builds the test image and runs every sandbox-tagged test in it, with the server's privileges and
# JUDGE_REQUIRE_SANDBOX=1 (a skipped test fails). See scripts/sandbox-test.sh.
sandbox-test:
	scripts/sandbox-test.sh

# Starts the shipped image over mutual TLS and talks to it with grpcurl. See scripts/smoke.sh.
smoke:
	GO_IMAGE=$(GO_IMAGE) scripts/smoke.sh
