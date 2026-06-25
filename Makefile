# CalDav — build / test / lint / package
#
# Override defaults on the command line, e.g.:
#   make image TAG=v1.0.0

IMAGE     ?= caldavproxy
TAG       ?= latest
IMAGE_REF := $(IMAGE):$(TAG)
DIST      ?= dist
ARTIFACT  := $(DIST)/$(IMAGE)-$(TAG).tar.gz
BIN       ?= bin/caldavproxy

.DEFAULT_GOAL := help

## help: show this help
.PHONY: help
help:
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'

## build: compile the binary into ./bin
.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) .

## test: run unit tests (no Docker)
.PHONY: test
test:
	go test ./...

## test-integration: run integration tests (requires Docker)
.PHONY: test-integration
test-integration:
	go test -tags integration ./...

## lint: run golangci-lint (falls back to go vet if not installed)
.PHONY: lint
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found, running 'go vet' instead"; \
		go vet ./...; \
	fi

## fmt: format the code
.PHONY: fmt
fmt:
	gofmt -w .

## tidy: tidy go.mod / go.sum
.PHONY: tidy
tidy:
	go mod tidy

## docker-build: build the Docker image ($(IMAGE_REF))
.PHONY: docker-build
docker-build:
	docker build -t $(IMAGE_REF) .

## docker-save: export the image to a portable gzip tarball ($(ARTIFACT))
.PHONY: docker-save
docker-save: docker-build
	@mkdir -p $(DIST)
	docker save $(IMAGE_REF) | gzip > $(ARTIFACT)
	@echo "Saved $(ARTIFACT)"

## image: build and package the image for transfer to another machine
.PHONY: image
image: docker-save

## clean: remove build artifacts
.PHONY: clean
clean:
	rm -rf bin $(DIST)
