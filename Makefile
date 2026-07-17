GO ?= go
DOCKER ?= docker
DOCKER_GO_IMAGE ?= golang:1.26-bookworm

BIN_DIR := $(CURDIR)/bin
PLUGIN_NAME := github-copilot-go
CLI_PROXY_API_DIR := $(abspath $(CURDIR)/../CLIProxyAPI)

LINUX_AMD64_DIR := $(BIN_DIR)/linux/amd64
LINUX_ARM64_DIR := $(BIN_DIR)/linux/arm64
LINUX_AMD64_PLUGIN := $(LINUX_AMD64_DIR)/$(PLUGIN_NAME).so
LINUX_ARM64_PLUGIN := $(LINUX_ARM64_DIR)/$(PLUGIN_NAME).so

LINUX_AMD64_CC ?= x86_64-linux-gnu-gcc
LINUX_ARM64_CC ?= aarch64-linux-gnu-gcc

NATIVE_GOOS := $(shell $(GO) env GOOS)
NATIVE_GOARCH := $(shell $(GO) env GOARCH)

ifeq ($(NATIVE_GOOS),windows)
NATIVE_EXT := dll
else ifeq ($(NATIVE_GOOS),darwin)
NATIVE_EXT := dylib
else
NATIVE_EXT := so
endif

NATIVE_DIR := $(BIN_DIR)/$(NATIVE_GOOS)/$(NATIVE_GOARCH)
NATIVE_PLUGIN := $(NATIVE_DIR)/$(PLUGIN_NAME).$(NATIVE_EXT)

.DEFAULT_GOAL := build

.PHONY: build build-linux-amd64 build-linux-arm64 build-native \
	_build-linux-amd64 _build-linux-arm64 _build-native \
	test vet integration clean

build: clean
	$(MAKE) _build-linux-amd64 _build-linux-arm64

build-linux-amd64: clean
	$(MAKE) _build-linux-amd64

build-linux-arm64: clean
	$(MAKE) _build-linux-arm64

build-native: clean
	$(MAKE) _build-native

_build-linux-amd64:
	mkdir -p $(LINUX_AMD64_DIR)
	@if command -v $(firstword $(LINUX_AMD64_CC)) >/dev/null 2>&1; then \
		CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC="$(LINUX_AMD64_CC)" $(GO) build -buildmode=c-shared -o $(LINUX_AMD64_PLUGIN) .; \
	elif command -v $(DOCKER) >/dev/null 2>&1; then \
		$(DOCKER) run --rm --platform linux/amd64 \
			-e CGO_ENABLED=1 -e GOOS=linux -e GOARCH=amd64 \
			-v "$(CURDIR):/workspace/cpa-github-copilot" \
			-v "$(CLI_PROXY_API_DIR):/workspace/CLIProxyAPI" \
			-w /workspace/cpa-github-copilot \
			$(DOCKER_GO_IMAGE) go build -buildmode=c-shared -o bin/linux/amd64/$(PLUGIN_NAME).so .; \
	else \
		echo "error: $(LINUX_AMD64_CC) or $(DOCKER) is required to build linux/amd64" >&2; \
		exit 1; \
	fi
	rm -f $(LINUX_AMD64_DIR)/$(PLUGIN_NAME).h

_build-linux-arm64:
	mkdir -p $(LINUX_ARM64_DIR)
	@if command -v $(firstword $(LINUX_ARM64_CC)) >/dev/null 2>&1; then \
		CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC="$(LINUX_ARM64_CC)" $(GO) build -buildmode=c-shared -o $(LINUX_ARM64_PLUGIN) .; \
	elif command -v $(DOCKER) >/dev/null 2>&1; then \
		$(DOCKER) run --rm --platform linux/arm64 \
			-e CGO_ENABLED=1 -e GOOS=linux -e GOARCH=arm64 \
			-v "$(CURDIR):/workspace/cpa-github-copilot" \
			-v "$(CLI_PROXY_API_DIR):/workspace/CLIProxyAPI" \
			-w /workspace/cpa-github-copilot \
			$(DOCKER_GO_IMAGE) go build -buildmode=c-shared -o bin/linux/arm64/$(PLUGIN_NAME).so .; \
	else \
		echo "error: $(LINUX_ARM64_CC) or $(DOCKER) is required to build linux/arm64" >&2; \
		exit 1; \
	fi
	rm -f $(LINUX_ARM64_DIR)/$(PLUGIN_NAME).h

_build-native:
	mkdir -p $(NATIVE_DIR)
	CGO_ENABLED=1 GOOS=$(NATIVE_GOOS) GOARCH=$(NATIVE_GOARCH) $(GO) build -buildmode=c-shared -o $(NATIVE_PLUGIN) .
	rm -f $(NATIVE_DIR)/$(PLUGIN_NAME).h

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

integration: clean
	$(MAKE) _build-native
	CPA_PLUGIN_INTEGRATION_BINARY=$(NATIVE_PLUGIN) $(GO) test -run '^TestBuiltPluginLoadsInCLIProxyHost$$' .

clean:
	rm -rf $(BIN_DIR)
