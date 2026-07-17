BIN_DIR := $(CURDIR)/bin
UNAME_S := $(shell uname -s)

ifeq ($(OS),Windows_NT)
PLUGIN_EXT := dll
else ifeq ($(UNAME_S),Darwin)
PLUGIN_EXT := dylib
else
PLUGIN_EXT := so
endif

PLUGIN := $(BIN_DIR)/github-copilot-go.$(PLUGIN_EXT)

.PHONY: build test vet integration clean

build: $(PLUGIN)

$(PLUGIN): $(wildcard *.go) go.mod go.sum | $(BIN_DIR)
	go build -buildmode=c-shared -o $@ .
	rm -f $(BIN_DIR)/github-copilot-go.h

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

test:
	go test ./...

vet:
	go vet ./...

integration: build
	CPA_PLUGIN_INTEGRATION_BINARY=$(PLUGIN) go test -run '^TestBuiltPluginLoadsInCLIProxyHost$$' .

clean:
	rm -rf $(BIN_DIR)
