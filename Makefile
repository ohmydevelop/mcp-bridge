SHELL := /bin/sh
.PHONY: build check release
build:
	CGO_ENABLED=0 go build -trimpath -o bin/mcp-bridge .
check: build
	go vet ./...
	go test -race ./...
	python3 tests/acceptance.py
	python3 tests/local_destinations.py
	python3 tests/qa_execute_script.py
	python3 tests/qa_trigger_replay.py
release:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/mcp-bridge-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/mcp-bridge-linux-arm64 .
	cd dist && sha256sum mcp-bridge-linux-* > SHA256SUMS

.PHONY: check-app capture
check-app: build
	python3 tests/app_smoke.py
CODEX ?= codex
capture: build
	python3 tests/codex_capture.py "$$(command -v $(CODEX))" bin/trace
