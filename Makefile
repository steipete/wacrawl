COVERAGE_THRESHOLD ?= 85.0

.DEFAULT_GOAL := help

.PHONY: help build test test-race fmt lint coverage deps snapshot test-release secrets sqlc check release verify-release release-artifacts clean

help:
	@printf '%s\n' \
		'Available targets:' \
		'  help              Print available targets (default).' \
		'  build             Build the CLI into bin/wacrawl.' \
		'  test              Run the full Go test suite.' \
		'  fmt               Check formatting with gofumpt.' \
		'  lint              Run every static-analysis gate enforced by CI.' \
		'  check             Run every local gate enforced by CI.' \
		'  snapshot          Build credential-free release artifacts.' \
		'  release           Refuse local publishing and print the official CI command.' \
		'  verify-release    Verify existing release artifacts (VERSION=vX.Y.Z).' \
		'  coverage          Run tests and enforce the coverage floor.' \
		'  test-race         Run the Go test suite with the race detector.' \
		'  deps              Verify modules, tidiness, and vulnerabilities.' \
		'  test-release      Test the macOS release scripts.' \
		'  secrets           Scan Git history and the working tree for secrets.' \
		'  sqlc              Regenerate sqlc output.' \
		'  release-artifacts Alias for release.' \
		'  clean             Remove local build output.'

build:
	mkdir -p bin
	GOWORK=off go build -o bin/wacrawl ./cmd/wacrawl

test:
	GOWORK=off go test -count=1 ./...

test-race:
	GOWORK=off go test -count=1 -race ./...

fmt:
	@set -e; \
	changed="$$(GOWORK=off go run mvdan.cc/gofumpt@v0.11.0 -l .)"; \
	if [ -n "$$changed" ]; then printf 'gofumpt wants changes in:\n%s\n' "$$changed"; exit 1; fi

lint:
	golangci-lint run ./...
	GOWORK=off go vet ./...
	GOWORK=off go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...
	@set -e; \
	output_file="$$(mktemp)"; \
	trap 'rm -f "$$output_file"' 0; \
	GOWORK=off go run golang.org/x/tools/cmd/deadcode@v0.49.0 -test ./... > "$$output_file"; \
	if [ -s "$$output_file" ]; then cat "$$output_file"; exit 1; fi
	GOWORK=off go run github.com/securego/gosec/v2/cmd/gosec@v2.29.0 -exclude=G101,G115,G202,G301,G304 ./...

coverage:
	GOWORK=off ./scripts/coverage.sh $(COVERAGE_THRESHOLD)

deps:
	GOWORK=off go mod verify
	GOWORK=off go mod tidy
	git diff --exit-code -- go.mod go.sum
	GOWORK=off go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...

snapshot:
	GOWORK=off goreleaser release --snapshot --clean --skip=publish

test-release:
	bash scripts/test-macos-release.sh

secrets:
	GOWORK=off go run github.com/zricethezav/gitleaks/v8@v8.30.1 git --no-banner --redact
	GOWORK=off go run github.com/zricethezav/gitleaks/v8@v8.30.1 dir . --no-banner --redact

sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate

check: fmt lint test test-race coverage build deps snapshot test-release secrets

release:
	@test -n "$(VERSION)" || (echo "usage: make release VERSION=vX.Y.Z" >&2; exit 2)
	@version="$(VERSION)"; version="$${version#v}"; \
	echo "local releases are disabled; official releases run in GitHub Actions" >&2; \
	echo "gh workflow run release-unified.yml --repo openclaw/wacrawl --ref main -f version=$$version" >&2; \
	exit 1

verify-release:
	@test -n "$(VERSION)" || (echo "usage: make verify-release VERSION=vX.Y.Z" >&2; exit 2)
	@release_version="$(VERSION)"; \
	./scripts/verify-macos-release.sh "$(VERSION)" \
		"dist/wacrawl_$${release_version#v}_darwin_arm64.tar.gz" \
		"dist/wacrawl_$${release_version#v}_darwin_amd64.tar.gz"

release-artifacts: release

clean:
	rm -rf bin
