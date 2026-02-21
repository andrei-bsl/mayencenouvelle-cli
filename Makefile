BINARY     := mayence
BUILD_DIR  := ./dist
CMD_DIR    := ./cmd
VERSION    ?= dev
LDFLAGS    := -ldflags "-X main.version=$(VERSION) -X main.buildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)"

.PHONY: all build test lint clean install help

## all: build and test
all: lint test build

## build: compile the mayence binary
build:
	@echo "Building $(BINARY)..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) $(CMD_DIR)/main.go
	@echo "✓ Built $(BUILD_DIR)/$(BINARY)"

## install: install binary to ~/go/bin
install:
	go install $(LDFLAGS) $(CMD_DIR)/main.go
	@echo "✓ Installed $(BINARY)"

## test: run all tests
test:
	go test ./... -v -count=1

## test-integration: run integration tests (requires running lab services)
test-integration:
	go test ./... -v -tags=integration

## lint: run golangci-lint
lint:
	golangci-lint run ./...

## clean: remove build artifacts
clean:
	@rm -rf $(BUILD_DIR)
	@echo "✓ Cleaned"

## run: run with args (e.g. make run ARGS="deploy nas-app")
run:
	go run $(CMD_DIR)/main.go $(ARGS)

## validate: validate all manifests
validate:
	go run $(CMD_DIR)/main.go validate

## help: display this help
help:
	@echo "Usage: make <target>"
	@grep -h "^##" $(MAKEFILE_LIST) | sed -e 's/## //'
