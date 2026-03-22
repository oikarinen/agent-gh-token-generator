# Contributing

We welcome contributions to this project! This guide provides instructions for how to get started with development, whether you are a human or an AI assistant.

## Development Workflow

This section outlines the standard workflow for building, testing, and modifying the application.

### Prerequisites

*   Go (version 1.21 or later)
*   `golangci-lint` for linting

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

## Contributing as an AI Assistant

This section provides specific guidance for an AI assistant like Gemini or Claude to contribute to this repository.

### 1. Understanding the Codebase

Start by exploring the repository structure. The key files are:

-   `go.mod`: Defines the Go module and its dependencies.
-   `cmd/gh-app-token-generator/main.go`: The main entry point of the application.
-   `internal/authtoken/authtoken.go`: Contains the core logic for generating the GitHub App token.
-   `bin/agent-github-token`: The user-facing shell script wrapper.

Use `list_directory` and `read_file` to understand the contents of these files.

### 2. Building and Testing in a Restricted Environment

If you are working in a restricted shell environment without a pre-configured Go toolchain, you may need to specify a local cache directory for Go modules:

**Building:**
```sh
# Create a local cache directory
mkdir -p .go/mod-cache

# Run the build command
GOMODCACHE=$(pwd)/.go/mod-cache go build -o bin/gh-app-token-generator ./cmd/gh-app-token-generator
```

**Testing:**
```sh
GOMODCACHE=$(pwd)/.go/mod-cache go test -v ./...
```

### 3. Making Modifications

To modify the code, use a `read-modify-write` cycle:

1.  **Read the file:** Use `read_file` to get the current content of the file you want to change.
2.  **Generate the new content:** Based on the user's request, generate the new code.
3.  **Apply the change:** Use `replace` to apply the changes to the file. Ensure you provide enough context in the `old_string` parameter to avoid ambiguous matches.

### Example Workflow: Adding a `--version` flag

Here’s a hypothetical example of how an AI assistant could add a `--version` flag to the application.

**1. Analyze the Request:** The user wants to add a `--version` flag that prints the version of the application and exits.

**2. Read the main file:**
`read_file('cmd/gh-app-token-generator/main.go')`

**3. Modify the code:**
In `main.go`, add a check for the `--version` flag.

```go
package main

import (
	"fmt"
	"os"

	"github.com/oikarinen/agent-gh-token-generator/internal/authtoken"
)

var version = "dev" // Can be set during build

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(version)
		os.Exit(0)
	}

	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "Usage: ", os.Args[0], " <app_id> <installation_id>")
		os.Exit(1)
	}
    // ... rest of the code
```
Use `replace` to apply this change.

**4. Rebuild the application:**
`go build -ldflags="-X main.version=1.1.0" -o bin/gh-app-token-generator ./cmd/gh-app-token-generator`

**5. Verify the change:**
`run_shell_command('./bin/gh-app-token-generator --version')`

The expected output would be `1.1.0`.

This structured approach allows an AI assistant to safely and effectively contribute to the development of this project.
