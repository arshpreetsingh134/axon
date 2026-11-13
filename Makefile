# axon — build, test and install targets.
# Author: Arshpreet Singh Bhatia

BIN      := bin/axon
PKG      := axon/internal/commands
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' | grep . || echo 0.1.0-dev)
COMMIT   ?= $(shell git rev-parse --verify -q HEAD 2>/dev/null || echo unknown)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
PREFIX   ?= $(HOME)/.local
VENV     := .venv

.PHONY: build dev undev install test test-go test-py test-e2e lint fmt snapshot clean completions

build: ## build ./bin/axon
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/axon

dev: build ## "editable" install: link $(PREFIX)/bin/axon → ./bin/axon (every `make build` is live)
	@mkdir -p $(PREFIX)/bin
	@ln -sf "$(CURDIR)/$(BIN)" "$(PREFIX)/bin/axon"
	@echo "linked $(PREFIX)/bin/axon -> $(CURDIR)/$(BIN)"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; *) echo "add $(PREFIX)/bin to PATH";; esac

undev: ## remove the dev link
	@rm -f "$(PREFIX)/bin/axon" && echo "removed $(PREFIX)/bin/axon"

install: ## go install into $GOBIN (a copy, not a link)
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/axon

test: test-go test-py ## all offline tests

test-go: ## Go unit tests
	go test ./...

$(VENV)/bin/pytest: pyproject.toml
	python3 -m venv $(VENV)
	$(VENV)/bin/pip install -q -e ".[test]"
	@touch $@

test-py: build $(VENV)/bin/pytest ## black-box pytest suite (Docker/K8s tests skipped)
	AXON_BIN=$(CURDIR)/$(BIN) $(VENV)/bin/pytest -q

test-e2e: build $(VENV)/bin/pytest ## end-to-end tests that start real containers (needs Docker)
	AXON_BIN=$(CURDIR)/$(BIN) $(VENV)/bin/pytest -q -m docker

lint: ## vet + format check
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; exit 1)

fmt:
	gofmt -w cmd internal

completions: build ## write shell completions into ./completions
	@mkdir -p completions
	$(BIN) completion bash > completions/axon.bash
	$(BIN) completion zsh  > completions/_axon
	$(BIN) completion fish > completions/axon.fish

snapshot: ## local GoReleaser dry run (no publish)
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist completions
