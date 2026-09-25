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
checks: vuln check-tidy

.PHONY: vuln
vuln:
	$(GO) tool govulncheck ./...

.PHONY: check-tidy
check-tidy:
	$(GO) mod tidy -diff
