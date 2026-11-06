# Development commands. Everything CI runs is a recipe here — the shared
# check workflow (truvity/ci-workflows) runs each one as its own job.

# Format Go files.
fmt:
    golangci-lint fmt ./...

# Compile check (library — nothing to run).
build:
    go build ./...

# Run unit tests.
test:
    go test ./... -coverprofile=coverage.out

# Run linters. `config verify` first: `run` accepts unknown top-level
# keys silently.
lint:
    golangci-lint config verify
    golangci-lint run ./...

# Reachable Go advisories (security.yaml, daily).
vuln:
    govulncheck ./...

# The reason this repository can be public. Runs in CI as its own job.
leak-canary:
    hack/leak-canary.sh

# The rename proof against the real Pulumi engine and GitHub provider
# (pkg/engine/rename_plan_test.go). Local only: it needs a `pulumi` CLI on
# PATH, which CI does not carry, and downloads the github provider plugin on
# first use. Nothing leaves the machine: the provider talks to a fake GitHub
# on 127.0.0.1. Without the CLI the Go tests would SKIP and exit 0, so this
# recipe refuses instead of reporting a pass it did not earn.
prove-rename:
    #!/usr/bin/env bash
    set -euo pipefail
    command -v pulumi >/dev/null || { echo "prove-rename: no pulumi CLI on PATH; install one (https://www.pulumi.com/docs/install/) and run again" >&2; exit 1; }
    out="$(mktemp)"
    trap 'rm -f "$out"' EXIT
    go test ./pkg/engine -run 'TestRenamePlan' -count=1 -v 2>&1 | tee "$out"
    if grep -q -- '--- SKIP' "$out"; then
        echo "prove-rename: a test skipped, so the proof did not run" >&2
        exit 1
    fi

# Run go mod tidy.
tidy:
    go mod tidy

# Clean build artifacts.
clean:
    rm -rf bin/ dist/ coverage.out

# Everything CI runs on a pull request.
check: build test lint leak-canary
