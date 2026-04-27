.PHONY: web-install test e2e start build-mac release-mac

WEB_DIR := web
GOARCH := $(shell go env GOARCH)
WAILS := go run github.com/wailsapp/wails/v2/cmd/wails@v2.12.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
APP_BUNDLE := build/bin/cursor-tap.app
RELEASE_DIR := build/release
RELEASE_ARCHIVE := $(RELEASE_DIR)/Cursor-Tap-$(VERSION)-macos-$(GOARCH).zip

web-install:
	cd $(WEB_DIR) && npm ci

test: web-install
	cd $(WEB_DIR) && npm run lint
	cd $(WEB_DIR) && npm run build
	go test ./...

e2e: web-install
	go test ./internal/e2e -run TestProxySQLiteWebSocketEndToEnd -v
	cd $(WEB_DIR) && npx playwright install chromium
	cd $(WEB_DIR) && npm run e2e

start:
	$(WAILS) dev

build-mac: web-install
	$(WAILS) build -platform darwin/$(GOARCH)

release-mac: build-mac
	rm -rf $(RELEASE_DIR)
	mkdir -p $(RELEASE_DIR)
	test -d "$(APP_BUNDLE)"
	ditto -c -k --sequesterRsrc --keepParent "$(APP_BUNDLE)" "$(RELEASE_ARCHIVE)"
	shasum -a 256 "$(RELEASE_ARCHIVE)" > "$(RELEASE_ARCHIVE).sha256"
	@echo "Created $(RELEASE_ARCHIVE)"
