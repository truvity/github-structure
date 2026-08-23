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

# Run go mod tidy.
tidy:
    go mod tidy

# Clean build artifacts.
clean:
    rm -rf bin/ dist/ coverage.out

# Everything CI runs on a pull request.
check: build test lint leak-canary vuln
