# Pacenote server — developer entry points. Every target is what CI runs.
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
export PATH := $(PATH):$(shell go env GOPATH)/bin

# The tools, each named once.
#
# The PATH above is not enough on its own. GNU Make runs a recipe line directly,
# without a shell, when the line holds no shell metacharacters — and that direct
# execution searches make's own PATH rather than the one exported here. A tool
# installed by `go install` is invisible to exactly the recipes that are a single
# command, and the error says "No such file or directory" rather than anything
# about PATH. `make fmt` failed that way while `make fmt-check`, which happens to
# use a shell, worked.
#
# Each is whatever is on PATH already, falling back to the Go bin directory, so
# a tool installed either way is found and neither shape of recipe cares.
GOBIN       := $(shell go env GOPATH)/bin
GOFUMPT     := $(shell command -v gofumpt      2>/dev/null || echo $(GOBIN)/gofumpt)
GOLANGCILINT:= $(shell command -v golangci-lint 2>/dev/null || echo $(GOBIN)/golangci-lint)
SQLC        := $(shell command -v sqlc         2>/dev/null || echo $(GOBIN)/sqlc)
GOVULNCHECK := $(shell command -v govulncheck  2>/dev/null || echo $(GOBIN)/govulncheck)
GORELEASER  := $(shell command -v goreleaser   2>/dev/null || echo $(GOBIN)/goreleaser)

MODULE    := github.com/pacenote-sim/server
BIN       := pacenote-server
CMD       := ./cmd/pacenote-server
DIST      := dist
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64
TESTFLAGS := -race -shuffle=on -count=1
PKGS      := ./...

# The database tests are behind a build tag and an environment variable,
# because Docker is not available everywhere. Point this at a PostgreSQL server
# the test user may create databases on:
#   make test-postgres PACENOTE_TEST_DATABASE_URL=postgres://you@localhost:5432/postgres
PACENOTE_TEST_DATABASE_URL ?=

# Release packaging. The container image compiles from source, and this module
# imports github.com/pacenote-sim/protocol and github.com/pacenote-sim/plugin,
# so the image's build context is the workspace directory above this repository
# rather than this one.
WORKSPACE := $(abspath ..)
IMAGE     ?= ghcr.io/pacenote-sim/pacenote-server
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
# syft signs the archives with a bill of materials in CI. Skipping it locally
# when it is not installed keeps `make dist` a one-command answer on a laptop.
SBOM_SKIP := $(shell command -v syft >/dev/null 2>&1 || echo ,sbom)

.DEFAULT_GOAL := check

.PHONY: help check build build-cross run vet lint lint-fix fmt fmt-check test test-race test-postgres test-dist cover sqlc tidy-check vuln dist dist-check release-dry release-notes image clean

## help: list targets
help:
	@grep -E '^## [a-z-]+:' $(MAKEFILE_LIST) | sed -E 's/^## ([a-z-]+): */\1\t/' | column -t -s $$'\t'

## check: everything CI runs, in order
check: fmt-check build build-cross vet lint test test-race tidy-check

## build: the server for this machine → ./$(BIN)
build:
	CGO_ENABLED=0 go build -trimpath -o $(BIN) $(CMD)

## build-cross: one static binary per supported platform → $(DIST)/
build-cross:
	mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
			-o $(DIST)/$(BIN)-$$os-$$arch$$ext $(CMD) || exit 1; \
	done

## run: build and start the server in ./$(DIST)/run, with its own data directory
run: build
	mkdir -p $(DIST)/run
	cp $(BIN) $(DIST)/run/
	cd $(DIST)/run && ./$(BIN)

## vet: go vet
vet:
	go vet $(PKGS)
	go vet -tags postgres $(PKGS)

## lint: golangci-lint, with and without the database build tag
lint:
	$(GOLANGCILINT) config verify
	$(GOLANGCILINT) run $(PKGS)
	$(GOLANGCILINT) run --build-tags postgres $(PKGS)

## lint-fix: apply auto-fixes and formatting
lint-fix:
	$(GOLANGCILINT) run --fix $(PKGS)
	$(GOFUMPT) -w .

## fmt: format
fmt:
	$(GOFUMPT) -w .

## fmt-check: fail if anything is unformatted
fmt-check:
	@out=$$($(GOFUMPT) -l .); if [ -n "$$out" ]; then echo "not gofumpt clean:"; echo "$$out"; exit 1; fi

## test: unit tests, no database needed
test:
	go test -shuffle=on -count=1 $(PKGS)

## test-race: tests as CI runs them
test-race:
	go test $(TESTFLAGS) $(PKGS)

## test-dist: the packaging tests, against the archives make dist produced
test-dist:
	@test -d $(DIST) || { echo "no $(DIST)/ yet — run make dist first"; exit 1; }
	go test -tags dist -count=1 ./internal/packaging/...

## test-postgres: the database tests, against PACENOTE_TEST_DATABASE_URL
test-postgres:
	@if [ -z "$(PACENOTE_TEST_DATABASE_URL)" ]; then \
		echo "set PACENOTE_TEST_DATABASE_URL to a PostgreSQL server you may create databases on"; exit 1; fi
	PACENOTE_TEST_DATABASE_URL=$(PACENOTE_TEST_DATABASE_URL) go test -tags postgres $(TESTFLAGS) $(PKGS)

## cover: coverage — every package over 90%, the server over 91%
cover:
	go test -tags postgres -covermode=atomic -coverpkg=./internal/... -coverprofile=coverage.out $(PKGS)
	scripts/coverage.py --profile coverage.out --min 90 --min-total 91

## sqlc: regenerate the typed queries from internal/db/queries
sqlc:
	$(SQLC) vet || true
	$(SQLC) generate

## tidy-check: fail if go.mod or go.sum would change
tidy-check:
	go mod tidy
	@git diff --quiet go.mod go.sum || { echo "go.mod or go.sum is not tidy"; exit 1; }

## vuln: known vulnerabilities in the dependency graph
vuln:
	$(GOVULNCHECK) $(PKGS)

## dist: the release package — one zip per platform, plus checksums.txt
dist:
	$(GORELEASER) release --snapshot --clean --skip=publish,announce$(SBOM_SKIP)
	@echo
	@ls -1 $(DIST)/*.zip $(DIST)/checksums.txt

## dist-check: dist, then the tests that read what it produced
dist-check: dist test-dist

## release-dry: rehearse a tagged release without tagging or publishing anything
release-dry:
	$(GORELEASER) check || echo "  (a configuration check needs a git remote; the release itself does not run without one)"
	$(MAKE) dist
	@echo
	@echo "release notes the newest CHANGELOG.md section would publish:"
	@./scripts/release-notes.sh latest | sed 's/^/  /'

## release-notes: the CHANGELOG.md section a tag would publish — make release-notes VERSION=v0.1.0
release-notes:
	@./scripts/release-notes.sh $(VERSION)

## image: the container image, built from the workspace above this repository
image:
	docker build \
		--file Dockerfile \
		--build-arg VERSION=$(VERSION) \
		--tag $(IMAGE):$(VERSION) \
		--tag $(IMAGE):latest \
		$(WORKSPACE)

## clean: remove build outputs
clean:
	rm -rf $(DIST) $(BIN) coverage.out
