# All Go tooling runs inside Docker: there is no local Go toolchain.
GO_IMAGE  ?= golang:1.23
BUF_IMAGE ?= bufbuild/buf:1.47.2
GO_RUN     = docker run --rm -v "$(CURDIR)":/src -v judge-server-gomod:/go/pkg/mod -w /src -e GOFLAGS=-mod=mod -e GOCACHE=/tmp/gocache $(GO_IMAGE)
VERSION   ?= dev

.PHONY: proto test vet build tidy

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
