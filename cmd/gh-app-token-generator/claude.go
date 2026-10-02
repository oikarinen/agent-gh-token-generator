package main

// Claude Code integration: hooks that confine a Claude Code session's GitHub
// access to the GitHub App token.
//
//   - The SessionStart hook writes a `gh` shim and points the session's git at
//     this tool, through environment variables that only the session sees.
//   - The PreToolUse hook blocks Bash commands that reach for other
//     credentials, and before commands that talk to GitHub it renews the
//     token and has GitHub confirm it belongs to the expected App.
//
// The hooks are guardrails. The hard boundary is Claude Code's sandbox, which
// `claude-settings` configures to hide credential files and variables.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/oikarinen/agent-gh-token-generator/internal/authtoken"
)

const gitCredentialKey = "credential.https://github.com.helper"

// tokenVariables are environment variables gh and other tools read GitHub
// tokens from. Sessions must not carry any of them.
var tokenVariables = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// credentialFiles hold other GitHub or SSH credentials.
var credentialFiles = []string{"~/.ssh", "~/.config/gh", "~/.git-credentials", "~/.netrc"}

// blockRules match Bash commands that reach for credentials other than the
// GitHub App token, or undo the session's setup.
var blockRules = []struct {
	pattern *regexp.Regexp
	reason  string
}{
	{regexp.MustCompile(`\bsecurity\s+(-\S+\s+)*(find-|dump-|export|import|unlock-|add-|delete-|set-|-i\b)`),
		"the macOS Keychain holds other credentials"},
	{regexp.MustCompile(`\bgh\s+auth\b`),
		"gh auth would use or change other GitHub logins; gh already uses the GitHub App token"},
	{regexp.MustCompile(`credential-(osxkeychain|store|cache|manager|libsecret|netrc)\b`),
		"other git credential helpers hold other credentials"},
	{regexp.MustCompile(`credential\.(helper|https?:)|insteadOf|GIT_CONFIG_|GIT_ASKPASS|SSH_ASKPASS|core\.askPass|core\.sshCommand|GIT_SSH`),
		"this changes how git authenticates"},
	{regexp.MustCompile(`(^|[^\w.-])\.ssh\b|\bssh-add\b|\bssh-agent\b|SSH_AUTH_SOCK`),
		"SSH keys authenticate as the user, not as the GitHub App"},
	{regexp.MustCompile(`\.config/gh\b|\.git-credentials\b|\.netrc\b|GH_CONFIG_DIR`),
		"this reaches other GitHub credentials"},
	{regexp.MustCompile(`\b(GH|GITHUB|GH_ENTERPRISE|GITHUB_ENTERPRISE)_TOKEN\s*=`),
		"gh and git get the GitHub App token automatically; setting a token by hand could use another one"},
	{regexp.MustCompile(`\benv\s+(-\S+\s+)*(-i\b|--ignore-environment\b)`),
		"clearing the environment would bypass the GitHub App token setup"},
}

// githubCommand matches commands that talk to GitHub: any gh command, and
// git commands that use a remote.
var githubCommand = regexp.MustCompile("(^|[\\s;&|(`])(gh(\\s|$)|git\\s+(\\S+\\s+)*(push|pull|fetch|clone|ls-remote|submodule|remote\\s+update)\\b)")

// claudeHook runs a Claude Code hook. A PreToolUse hook fails closed: exit
// status 2 blocks the tool call, while other failures would let it through.
func (c *cli) claudeHook(ctx context.Context, event, clientID string) int {
	switch event {
	case "session-start":
		if clientID == "" {
			return c.fail(errors.New("claude-hook needs --client-id"))
		}
		return c.sessionStart(clientID)
	case "pre-tool-use":
		if clientID == "" {
			fmt.Fprintln(c.stderr, "agent-github-token: the pre-tool-use hook needs --client-id; blocking until it is configured")
			return 2
		}
		return c.preToolUse(ctx, clientID)
	default:
		fmt.Fprintf(c.stderr, "Unknown hook event %q\n\n%s", event, usage)
		return 1
	}
}

// sessionStart writes the gh shim and adds the session's environment to
// $CLAUDE_ENV_FILE, which Claude Code applies to every Bash command.
func (c *cli) sessionStart(clientID string) int {
	_, _ = io.Copy(io.Discard, c.stdin) // the hook input isn't needed

	envFile := c.getenv("CLAUDE_ENV_FILE")
	if envFile == "" {
		return c.fail(errors.New("CLAUDE_ENV_FILE is not set; run this as a Claude Code SessionStart hook"))
	}
	helper, err := c.executable()
	if err != nil {
		return c.fail(err)
	}
	cacheDir, err := c.cacheDir()
	if err != nil {
		return c.fail(err)
	}
	shimDir := filepath.Join(cacheDir, "agent-github-token", "claude-bin")
	realGh := findExecutable("gh", c.getenv("PATH"), shimDir)

	if err := writeFileAtomic(filepath.Join(shimDir, "gh"), []byte(ghShim(helper, clientID, realGh)), 0o755); err != nil {
		return c.fail(fmt.Errorf("writing the gh shim: %w", err))
	}
	f, err := os.OpenFile(envFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return c.fail(err)
	}
	if _, err := f.WriteString(sessionEnv(shimDir, helper, clientID)); err != nil {
		_ = f.Close()
		return c.fail(err)
	}
	if err := f.Close(); err != nil {
		return c.fail(err)
	}

	return c.writeJSON(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "SessionStart",
		"additionalContext": fmt.Sprintf("GitHub access in this session goes through the agent-github-token GitHub App "+
			"(client ID %s): gh and git over HTTPS use its token automatically, and git@github.com remotes are rewritten "+
			"to HTTPS. It reaches only the repositories the App is installed on. Don't use other GitHub credentials or set "+
			"GH_TOKEN or GITHUB_TOKEN yourself; gh auth is disabled. If a command reports that the GitHub App login is "+
			"missing or expired, ask the user to run `agent-github-token login` in their own terminal.", clientID),
	}})
}

// preToolUse checks a Bash command before it runs.
func (c *cli) preToolUse(ctx context.Context, clientID string) int {
	var input struct {
		ToolName  string `json:"tool_name"`
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.NewDecoder(c.stdin).Decode(&input); err != nil {
		fmt.Fprintf(c.stderr, "agent-github-token: cannot read the hook input: %v\n", err)
		return 2
	}
	if input.ToolName != "Bash" {
		return 0
	}
	command := input.ToolInput.Command

	for _, rule := range blockRules {
		if rule.pattern.MatchString(command) {
			return c.deny(rule.reason)
		}
	}
	if cacheDir, err := c.cacheDir(); err == nil {
		shimDir := filepath.Join(cacheDir, "agent-github-token", "claude-bin")
		if realGh := findExecutable("gh", c.getenv("PATH"), shimDir); realGh != "" && strings.Contains(command, realGh) {
			return c.deny("this runs gh directly, without the GitHub App token; run plain `gh`")
		}
	}

	if githubCommand.MatchString(command) {
		service, err := c.newService(clientID)
		if err != nil {
			fmt.Fprintf(c.stderr, "agent-github-token: %v\n", err)
			return 2
		}
		// Renew here, outside the sandbox, and have GitHub confirm the App.
		if _, err := service.VerifiedToken(ctx); err != nil {
			return c.deny(tokenProblem(err))
		}
	}
	return 0
}

func tokenProblem(err error) string {
	switch {
	case errors.Is(err, authtoken.ErrNotLoggedIn), errors.Is(err, authtoken.ErrLoginExpired):
		return fmt.Sprintf("the GitHub App login is missing or expired (%v). Ask the user to run `agent-github-token login` in their own terminal.", err)
	case errors.Is(err, authtoken.ErrClientIDMismatch):
		return fmt.Sprintf("the stored GitHub token is not from the expected GitHub App (%v). Ask the user to run `agent-github-token login` in their own terminal.", err)
	default:
		return fmt.Sprintf("could not get a GitHub App token: %v", err)
	}
}

func (c *cli) deny(reason string) int {
	return c.writeJSON(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": "agent-github-token: " + reason,
	}})
}

func (c *cli) writeJSON(v any) int {
	if err := json.NewEncoder(c.stdout).Encode(v); err != nil {
		return c.fail(err)
	}
	return 0
}

// ghShim returns the gh replacement put first on the session's PATH. It gets
// a token for clientID and runs the real gh with it, with a gh config
// directory of its own so gh can't fall back to other stored logins.
func ghShim(helper, clientID, realGh string) string {
	var b strings.Builder
	b.WriteString(`#!/bin/bash
# Written by agent-github-token at the start of each Claude Code session.
# Runs gh with the GitHub App token and no other GitHub credentials.
set -euo pipefail
if [[ "${1:-}" == auth ]]; then
  echo "agent-github-token: gh auth is disabled in this session; gh already uses the GitHub App token." >&2
  exit 1
fi
`)
	if realGh == "" {
		b.WriteString("echo \"agent-github-token: gh is not installed.\" >&2\nexit 1\n")
		return b.String()
	}
	fmt.Fprintf(&b, "token=\"$(%s token --client-id %s)\"\n", shellQuote(helper), shellQuote(clientID))
	fmt.Fprintf(&b, "unset %s\n", strings.Join(tokenVariables[1:], " "))
	b.WriteString(`export GH_CONFIG_DIR="${TMPDIR:-/tmp}/agent-github-token-gh"` + "\n")
	b.WriteString("export GH_NO_UPDATE_NOTIFIER=1\n")
	fmt.Fprintf(&b, "GH_TOKEN=\"${token}\" exec %s \"$@\"\n", shellQuote(realGh))
	return b.String()
}

// sessionEnv returns the shell statements that set up a session: the gh shim
// first on PATH, no token variables, and git configured through
// GIT_CONFIG_COUNT (which only this session sees) to get github.com
// credentials from this tool alone and to use HTTPS for SSH remotes.
func sessionEnv(shimDir, helper, clientID string) string {
	gitConfig := [][2]string{
		{gitCredentialKey, ""}, // an empty helper forgets those configured elsewhere
		{gitCredentialKey, "!" + shellQuote(helper) + " git-credential --client-id " + shellQuote(clientID)},
		{"url.https://github.com/.insteadOf", "git@github.com:"},
		{"url.https://github.com/.insteadOf", "ssh://git@github.com/"},
	}
	var b strings.Builder
	b.WriteString("# agent-github-token: confine GitHub access to the GitHub App token\n")
	fmt.Fprintf(&b, "export PATH=%s:\"$PATH\"\n", shellQuote(shimDir))
	fmt.Fprintf(&b, "unset %s\n", strings.Join(tokenVariables, " "))
	fmt.Fprintf(&b, "export GIT_CONFIG_COUNT=%d\n", len(gitConfig))
	for i, entry := range gitConfig {
		fmt.Fprintf(&b, "export GIT_CONFIG_KEY_%d=%s GIT_CONFIG_VALUE_%d=%s\n", i, shellQuote(entry[0]), i, shellQuote(entry[1]))
	}
	b.WriteString("export GIT_TERMINAL_PROMPT=0\n")
	return b.String()
}

// claudeSettings prints the Claude Code user settings for the hooks and a
// sandbox that hides other credentials.
func (c *cli) claudeSettings(clientID string) int {
	helper, err := c.executable()
	if err != nil {
		return c.fail(err)
	}
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type hookGroup struct {
		Matcher string `json:"matcher,omitempty"`
		Hooks   []hook `json:"hooks"`
	}
	type fileRule struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	type envRule struct {
		Name string `json:"name"`
		Mode string `json:"mode"`
	}
	hookCommand := func(event string) string {
		return shellQuote(helper) + " claude-hook " + event + " --client-id " + shellQuote(clientID)
	}

	var readDenies []string
	var files []fileRule
	for _, path := range credentialFiles {
		readDenies = append(readDenies, "Read("+path+")", "Read("+path+"/**)")
		files = append(files, fileRule{Path: path, Mode: "deny"})
	}
	var envVars []envRule
	for _, name := range tokenVariables {
		envVars = append(envVars, envRule{Name: name, Mode: "deny"})
	}

	settings := struct {
		Hooks       map[string][]hookGroup `json:"hooks"`
		Permissions map[string][]string    `json:"permissions"`
		Sandbox     map[string]any         `json:"sandbox"`
	}{
		Hooks: map[string][]hookGroup{
			"SessionStart": {{Hooks: []hook{{Type: "command", Command: hookCommand("session-start")}}}},
			"PreToolUse":   {{Matcher: "Bash", Hooks: []hook{{Type: "command", Command: hookCommand("pre-tool-use")}}}},
		},
		Permissions: map[string][]string{"deny": readDenies},
		Sandbox: map[string]any{
			"enabled":                  true,
			"allowUnsandboxedCommands": false,
			"failIfUnavailable":        true,
			// gh is written in Go, which needs the macOS trust service to
			// verify TLS certificates inside the sandbox.
			"enableWeakerNetworkIsolation": true,
			"network": map[string]any{
				"allowedDomains": []string{"github.com", "*.github.com", "*.githubusercontent.com"},
			},
			"credentials": map[string]any{"files": files, "envVars": envVars},
		},
	}
	encoder := json.NewEncoder(c.stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(settings); err != nil {
		return c.fail(err)
	}
	fmt.Fprintln(c.stderr, "Merge these settings into ~/.claude/settings.json, keeping any domains and rules you already have.")
	return 0
}

// findExecutable looks for name in pathList, skipping skipDir (the shim's
// own directory).
func findExecutable(name, pathList, skipDir string) string {
	for _, dir := range filepath.SplitList(pathList) {
		if dir == "" || filepath.Clean(dir) == filepath.Clean(skipDir) {
			continue
		}
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path
		}
	}
	return ""
}

// writeFileAtomic replaces path so a concurrent reader never sees half a file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// shellQuote quotes s for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
