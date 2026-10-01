# Contributing

We welcome contributions to this project! This guide provides instructions for how to get started with development, whether you are a human or an AI assistant.

## Development Workflow

This section outlines the standard workflow for building, testing, and modifying the application.

### Prerequisites

*   Go (version 1.26 or later)
*   `golangci-lint` for linting
*   `shellcheck` for the wrapper script
*   `goreleaser` (optional) for checking the release build

### Building the Code

To compile the application, run the following command from the repository root:

```sh
go build -o bin/gh-app-token-generator ./cmd/gh-app-token-generator
```

### Running Tests

To run the test suite, use the `go test` command:

```sh
go test -v ./...
```

### Linting

We use `golangci-lint` to enforce code style and quality. To run the linter locally, use:

```sh
golangci-lint run
```

### Testing the Wrapper Script

The wrapper tests run `bin/agent-github-token` against stub versions of the helper and `gh`, so they need no GitHub App or Keychain:

```sh
shellcheck bin/agent-github-token test/*.sh
test/agent-github-token_test.sh
```

### Checking the Release Build

CI validates the GoReleaser config and builds a snapshot on every pull request. To do the same locally:

```sh
goreleaser check
goreleaser release --snapshot --clean
```

### GitHub Actions Style Guide

To ensure the security and stability of our CI/CD pipelines, all GitHub Actions used in workflow files must be pinned to a specific commit SHA.

**Rule:** Always use the full-length commit SHA for an action, and include a trailing comment indicating the major version tag for readability.

**Example:**

```yaml
# Correct: Pinned to a specific commit SHA
- uses: actions/checkout@a5ac7e51b41094c92402da3b24376905380afc29 # v4

# Incorrect: Using a mutable tag
# - uses: actions/checkout@v4
```

This practice prevents your workflow from breaking unexpectedly or being compromised if a version tag is moved to a different commit.

