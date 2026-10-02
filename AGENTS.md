# Contributing as an AI Assistant

This section provides specific guidance for an AI assistant like Gemini or Claude to contribute to this repository.

## 1. Understanding the Codebase

Start by exploring the repository structure. The key files are:

-   `go.mod`: Defines the Go module and its dependencies.
-   `cmd/gh-app-token-generator/main.go`: The command-line interface (`login`, `token`, `status`, `logout`, `git-credential`).
-   `cmd/gh-app-token-generator/claude.go`: Claude Code hooks and settings that confine a session's GitHub access to the App token.
-   `internal/authtoken/authtoken.go`: Device flow login and automatic token renewal.
-   `internal/authtoken/github.go`: Calls to GitHub's OAuth endpoints.
-   `internal/authtoken/keychain.go`: Token storage in the macOS Keychain.
-   `bin/agent-github-token`: The user-facing shell script wrapper.
-   `test/agent-github-token_test.sh`: Tests for the wrapper script.

Use `list_directory` and `read_file` to understand the contents of these files.

## 2. Building and Testing in a Restricted Environment

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

## 3. Making Modifications

To modify the code, use a `read-modify-write` cycle:

1.  **Read the file:** Use `read_file` to get the current content of the file you want to change.
2.  **Generate the new content:** Based on the user's request, generate the new code.
3.  **Apply the change:** Use `replace` to apply the changes to the file. Ensure you provide enough context in the `old_string` parameter to avoid ambiguous matches.

## Example Workflow: Adding a `--version` flag

Here’s a hypothetical example of how an AI assistant could add a `--version` flag to the application.

**1. Analyze the Request:** The user wants to add a `--version` flag that prints the version of the application and exits.

**2. Read the main file:**
`read_file('cmd/gh-app-token-generator/main.go')`

**3. Modify the code:**
In `main.go`, add a check for the `--version` flag.

```go
var version = "dev" // Can be set during build

func (c *cli) run(ctx context.Context, args []string) int {
	// ... argument count check

	switch command {
	case "--version":
		fmt.Fprintln(c.stdout, version)
		return 0
	case "help", "-h", "--help":
	// ... rest of the code
```
Use `replace` to apply this change.

**4. Rebuild the application:**
`go build -ldflags="-X main.version=1.1.0" -o bin/gh-app-token-generator ./cmd/gh-app-token-generator`

**5. Verify the change:**
`run_shell_command('./bin/gh-app-token-generator --version')`

The expected output would be `1.1.0`.

This structured approach allows an AI assistant to safely and effectively contribute to the development of this project.
