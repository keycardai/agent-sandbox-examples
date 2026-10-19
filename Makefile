# Every tool runs through `go tool` at the version go.mod pins.

GO ?= go
export GOTOOLCHAIN ?= auto

.PHONY: all
all: build test lint checks

.PHONY: build
build:
	$(GO) build ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test: vet
	$(GO) test -race ./...

.PHONY: lint
lint:
	$(GO) tool golangci-lint run ./...

.PHONY: fmt
fmt:
	$(GO) tool golangci-lint fmt ./...

.PHONY: checks
checks: vuln check-tidy check-licenses check-notice

.PHONY: vuln
vuln:
	$(GO) tool govulncheck ./...

.PHONY: check-tidy
check-tidy:
	$(GO) mod tidy -diff

.PHONY: check-licenses
check-licenses:
	GO=$(GO) ./scripts/check-licenses.sh

.PHONY: check-notice
check-notice:
	GO=$(GO) ./scripts/notice.sh --check

.PHONY: notice
notice:
	GO=$(GO) ./scripts/notice.sh
