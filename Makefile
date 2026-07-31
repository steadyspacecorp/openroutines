# Make configuration
.DEFAULT_GOAL := build
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
MAKEFLAGS += --warn-undefined-variables
.DELETE_ON_ERROR:

.PHONY: build lint fix tidy test test-race smoke check check-vuln check-tidy verify release clean

# Build
build:
	go build -o bin/openroutines ./cmd/openroutines

# Lint & format
LINT := golangci-lint run

lint:
	$(LINT)

fix:
	$(LINT) --fix

tidy:
	go mod tidy

# Test
test:
	go test ./...

test-race:
	go test -race ./...

smoke:
	@bin/smoke

# Checks (CI gates the golangci-lint action doesn't already cover)
check: check-vuln check-tidy

check-vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

# Compares tidy's output against the current files rather than against git, so
# an uncommitted-but-tidy go.mod passes and only real drift fails.
check-tidy:
	@cp go.mod go.mod.tidybak && cp go.sum go.sum.tidybak; \
	trap 'mv go.mod.tidybak go.mod; mv go.sum.tidybak go.sum' EXIT; \
	go mod tidy; \
	diff -u go.mod.tidybak go.mod && diff -u go.sum.tidybak go.sum \
	  || { echo "go.mod/go.sum are not tidy; run: make tidy" >&2; exit 1; }

# Everything CI runs, in one target. No `build`: test-race compiles every
# package, so a separate build step would only repeat it.
verify: lint check test-race smoke

# Release & cleanup
release:
	@bin/release

clean:
	rm -rf bin/openroutines dist
