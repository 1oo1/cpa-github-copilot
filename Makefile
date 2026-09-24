GO ?= go
BUN ?= bun
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
.DEFAULT_GOAL := build
.PHONY: web build test vet race integration verify release clean
web:
	cd web && $(BUN) install --frozen-lockfile && $(BUN) run build
build: web
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/copilot-proxy ./cmd/copilot-proxy
test:
	$(GO) test ./...
vet:
	$(GO) vet ./...
race:
	$(GO) test -race ./...
integration:
	$(GO) test ./internal/app -run 'TestStandalone|TestBudget|TestStream' -count=1
verify: test vet race
	cd web && $(BUN) run verify
release: web
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/copilot-proxy-linux-amd64 ./cmd/copilot-proxy
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/copilot-proxy-linux-arm64 ./cmd/copilot-proxy
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/copilot-proxy-darwin-arm64 ./cmd/copilot-proxy
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/copilot-proxy-windows-amd64.exe ./cmd/copilot-proxy
clean:
	$(GO) clean ./...
