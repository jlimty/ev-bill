# Tools installed with `go install` (govulncheck, gremlins) live in GOPATH/bin,
# which is not always on PATH, so they are run by full path. Putting this
# directory on PATH inside the Makefile is not enough: GNU Make 3.81 (the macOS
# default) starts simple commands itself, using the PATH it was launched with.
GOPATH_BIN := $(shell go env GOPATH)/bin

# Integration tests use this database. Override it from the environment if needed.
TEST_DATABASE_URL ?= postgres://localhost:5432/evbilling_test?sslmode=disable
export TEST_DATABASE_URL

.PHONY: fmt-check lint test test-tz vuln secrets mutation check run

# Fails if gofmt would change any file.
fmt-check:
	@unformatted="$$(gofmt -l .)" || exit 1; \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

lint:
	go vet ./...
	golangci-lint run

test:
	go test -race -covermode=atomic -coverpkg=./internal/... -coverprofile=coverage.out ./...
	scripts/coverage-gate.sh

# -count=1 matters: Go's test cache does not notice a changed TZ, so without it
# the second run could replay the first run's result and prove nothing.
# The skip applies only while internal/billing does not exist; once it does,
# the tests run for real.
test-tz:
	@if [ -d internal/billing ]; then \
		TZ=UTC go test -count=1 ./internal/billing/... && \
		TZ=America/New_York go test -count=1 ./internal/billing/...; \
	else \
		echo "test-tz: skipped, internal/billing does not exist yet"; \
	fi

vuln:
	$(GOPATH_BIN)/govulncheck ./...

# Two scans: committed history, and the working tree. The history scan cannot
# see uncommitted files, so without the second scan a secret could be caught
# only after it was committed. With no commits yet the history scan finds
# nothing and passes. The working-tree scan skips git-ignored files, so it uses
# its own config; the history scan keeps gitleaks' default rules.
secrets:
	gitleaks git --redact -v
	gitleaks dir --redact -v --config scripts/gitleaks-worktree.toml

# Reports the mutation score. Not part of `check`.
mutation:
	@if [ -d internal/billing ]; then \
		$(GOPATH_BIN)/gremlins unleash ./internal/billing; \
	else \
		echo "mutation: skipped, internal/billing does not exist yet"; \
	fi

check: fmt-check lint test test-tz vuln secrets

# Runs the server against the local evbilling database. cmd/server arrives in
# Task 10, so this fails until then.
run:
	PORT=8080 DATABASE_URL='postgres://localhost:5432/evbilling?sslmode=disable' API_KEY=dev go run ./cmd/server
