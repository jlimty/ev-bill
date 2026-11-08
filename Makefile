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

# Two profiles. coverage.out covers all of internal/... from every package's
# tests: it feeds the 80% total gate and is the SonarQube input.
# coverage-billing.out comes from internal/billing's own tests only, so the 95%
# billing gate cannot be met by other packages exercising billing code.
test:
	go test -race -covermode=atomic -coverpkg=./internal/... -coverprofile=coverage.out ./...
	@if [ -d internal/billing ]; then \
		go test -race -covermode=atomic -coverprofile=coverage-billing.out ./internal/billing; \
	fi
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
	go tool govulncheck ./...

# Two scans: committed history, and the working tree. The history scan cannot
# see uncommitted files, so without the second scan a secret could be caught
# only after it was committed. With no commits yet the history scan finds
# nothing and passes. The working-tree scan skips git-ignored files, so it uses
# its own config; the history scan keeps gitleaks' default rules.
# gitleaks is pinned in its own module, tools/gitleaks/go.mod, which Dependabot
# updates; keeping it out of the root go.mod keeps its dependencies out of the
# service's module graph. Go builds it from source through the module proxy and
# checks every download against tools/gitleaks/go.sum.
GITLEAKS := go tool -modfile=tools/gitleaks/go.mod gitleaks
secrets:
	$(GITLEAKS) git --redact -v
	$(GITLEAKS) dir --redact -v --config scripts/gitleaks-worktree.toml

# Reports the mutation score. Not part of `check`.
# gremlins sets each mutant's timeout to the time of its first coverage run
# times --timeout-coefficient. This suite runs in about 0.1 s, less than one
# compile, so the default coefficient made every mutant "time out" and the
# score read 0%. 50 leaves room for a compile; a mutant that loops forever
# still ends as TIMED OUT after a few seconds.
mutation:
	@if [ -d internal/billing ]; then \
		go tool gremlins unleash --timeout-coefficient 50 ./internal/billing; \
	else \
		echo "mutation: skipped, internal/billing does not exist yet"; \
	fi

check: fmt-check lint test test-tz vuln secrets

# Runs the server against the local evbilling database on port 8080 with API_KEY=dev.
run:
	PORT=8080 DATABASE_URL='postgres://localhost:5432/evbilling?sslmode=disable' API_KEY=dev go run ./cmd/server
