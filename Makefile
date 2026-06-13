BINARY     := cdbct
MODULE     := github.com/joshdurbin/cockroach_testing
BUILD_DIR  := .
GOBIN      ?= $(shell go env GOPATH)/bin

.PHONY: all build install clean generate tidy lint fmt docker-clean help

all: build

## build: compile the binary to ./cdbct
build:
	go build -o $(BUILD_DIR)/$(BINARY) .

## install: build and place the binary in GOPATH/bin
install:
	go install .

## proto: regenerate gRPC stubs from proto/cdbct/v1/tenant.proto
proto:
	PATH="$$PATH:$$HOME/go/bin" buf generate

## generate: regenerate sqlc query code from schema + query files
generate:
	sqlc generate

## tidy: tidy and verify go modules
tidy:
	go mod tidy
	go mod verify

## fmt: format all Go source files
fmt:
	gofmt -w -s .

## lint: run go vet
lint:
	go vet ./...

## clean: remove build artifacts
clean:
	rm -f $(BUILD_DIR)/$(BINARY)

## docker-clean: remove all Docker containers, images, networks, build cache, and volumes
docker-clean:
	docker system prune -af --volumes
	-docker volume rm $$(docker volume ls -q) 2>/dev/null || true

## help: print this help
help:
	@grep -E '^##' Makefile | sed 's/^## //'
