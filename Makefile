SHELL := /bin/sh
.PHONY: build check release
build:
	CGO_ENABLED=0 go build -trimpath -o bin/mcp-bridge .
check: build
	go vet ./...
	go test -race ./...
	python3 tests/standard_relay.py
	python3 tests/source_oauth.py
	python3 tests/acceptance.py
	python3 tests/local_destinations.py
	python3 tests/trigger_intent.py
	python3 tests/qa_execute_script.py
	python3 tests/qa_trigger_replay.py
release:
	python3 deploy/package.py

.PHONY: check-app capture
check-app: build
	python3 tests/app_smoke.py
CODEX ?= codex
capture: build
	python3 tests/codex_capture.py "$$(command -v $(CODEX))" bin/trace
