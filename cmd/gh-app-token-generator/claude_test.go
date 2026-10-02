package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oikarinen/agent-gh-token-generator/internal/authtoken"
)

// writeScript writes an executable bash script.
func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// hookEnv sets up a cache directory, a CLAUDE_ENV_FILE and a PATH with a
// stand-in gh, as a Claude Code session would have them.
type hookEnv struct {
	cacheDir, envFile, binDir, realGh string
	env                               map[string]string
}

func newHookEnv(t *testing.T) *hookEnv {
	dir := t.TempDir()
	h := &hookEnv{
		cacheDir: filepath.Join(dir, "cache"),
		envFile:  filepath.Join(dir, "claude-env"),
		binDir:   filepath.Join(dir, "bin"),
	}
	h.realGh = filepath.Join(h.binDir, "gh")
	writeScript(t, h.realGh, "exit 0")
	h.env = map[string]string{"CLAUDE_ENV_FILE": h.envFile, "PATH": h.binDir + ":/usr/bin:/bin"}
	return h
}

func (h *hookEnv) shimDir() string {
	return filepath.Join(h.cacheDir, "agent-github-token", "claude-bin")
}

func (h *hookEnv) run(service *fakeService, stdin string, args ...string) result {
	c, stdout, stderr := newTestCLI(service, stdin, h.env, h.cacheDir)
	code := c.run(context.Background(), args)
	return result{code, stdout.String(), stderr.String()}
}

func TestSessionStart(t *testing.T) {
	h := newHookEnv(t)
	if err := os.WriteFile(h.envFile, []byte("export FROM_ANOTHER_HOOK=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := h.run(&fakeService{}, `{"hook_event_name":"SessionStart","source":"startup"}`,
		"claude-hook", "session-start", "--client-id", "Iv23liTest")
	if r.code != 0 {
		t.Fatalf("Exit code = %d, stderr %q", r.code, r.stderr)
	}

	var out struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v: %q", err, r.stdout)
	}
	if out.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(out.HookSpecificOutput.AdditionalContext, "Iv23liTest") ||
		!strings.Contains(out.HookSpecificOutput.AdditionalContext, "agent-github-token login") {
		t.Errorf("Unexpected hook output: %+v", out)
	}

	shim := filepath.Join(h.shimDir(), "gh")
	info, err := os.Stat(shim)
	if err != nil {
		t.Fatalf("Shim not written: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("Shim mode = %v, want 0755", info.Mode().Perm())
	}
	shimText, _ := os.ReadFile(shim)
	for _, want := range []string{shellQuote(testHelperPath) + " token --client-id 'Iv23liTest'", "exec " + shellQuote(h.realGh)} {
		if !strings.Contains(string(shimText), want) {
			t.Errorf("Shim does not contain %q:\n%s", want, shimText)
		}
	}

	// Apply the env file the way Claude Code does and look at the result
	// through bash and the real git, with git's own config isolated.
	home := t.TempDir()
	script := `source "$1"
echo "FROM_ANOTHER_HOOK=${FROM_ANOTHER_HOOK:-}"
echo "PATH=$PATH"
echo "GH_TOKEN=${GH_TOKEN:-unset} GITHUB_TOKEN=${GITHUB_TOKEN:-unset}"
git config --get-all credential.https://github.com.helper | sed 's/^/helper=[/; s/$/]/'
git config --get-all url.https://github.com/.insteadOf | sed 's/^/insteadOf=/'
`
	cmd := exec.Command("bash", "-c", script, "bash", h.envFile)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin", "HOME=" + home,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
		"GH_TOKEN=ghp_personal", "GITHUB_TOKEN=ghp_personal"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Applying the env file failed: %v\n%s", err, output)
	}
	for _, want := range []string{
		"FROM_ANOTHER_HOOK=1",
		"PATH=" + h.shimDir() + ":/usr/bin:",
		"GH_TOKEN=unset GITHUB_TOKEN=unset",
		"helper=[]",
		"helper=[!" + shellQuote(testHelperPath) + " git-credential --client-id 'Iv23liTest']",
		"insteadOf=git@github.com:",
		"insteadOf=ssh://git@github.com/",
	} {
		if !strings.Contains(string(output), want) {
			t.Errorf("Session environment does not contain %q:\n%s", want, output)
		}
	}
}

func TestSessionStartErrors(t *testing.T) {
	h := newHookEnv(t)
	h.env["CLAUDE_ENV_FILE"] = ""
	h.run(&fakeService{}, "{}", "claude-hook", "session-start", "--client-id", "Iv23liTest").check(t, 1, "", "CLAUDE_ENV_FILE")

	h = newHookEnv(t)
	h.run(&fakeService{}, "{}", "claude-hook", "session-start").check(t, 1, "", "needs --client-id")
	h.run(&fakeService{}, "{}", "claude-hook", "teardown", "--client-id", "Iv23liTest").check(t, 1, "", "Unknown hook event")
}

func TestGhShim(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	writeScript(t, helper, `if [[ "$*" == "token --client-id Iv23liTest" ]]; then printf ghu_app; else echo "Error: wrong App" >&2; exit 1; fi`)
	realGh := filepath.Join(dir, "real-gh")
	writeScript(t, realGh, `printf '%s\n' "$@" > "$OUT/args"
echo "GH_TOKEN=${GH_TOKEN:-} GITHUB_TOKEN=${GITHUB_TOKEN:-} GH_CONFIG_DIR=${GH_CONFIG_DIR:-}" > "$OUT/env"`)

	// runShim runs the generated shim and returns the directory the stand-in
	// gh writes to, and the shim's combined output.
	runShim := func(t *testing.T, clientID string, args ...string) (outDir, output string, err error) {
		t.Helper()
		shim := filepath.Join(t.TempDir(), "gh")
		writeScript(t, shim, strings.TrimPrefix(ghShim(helper, clientID, realGh), "#!/bin/bash\n"))
		outDir = t.TempDir()
		cmd := exec.Command(shim, args...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "OUT=" + outDir, "TMPDIR=" + outDir, "GITHUB_TOKEN=ghp_personal", "GH_ENTERPRISE_TOKEN=x"}
		combined, err := cmd.CombinedOutput()
		return outDir, string(combined), err
	}

	t.Run("runs the real gh with the App token only", func(t *testing.T) {
		out, output, err := runShim(t, "Iv23liTest", "pr", "create", "--title", "two words")
		if err != nil {
			t.Fatalf("Shim failed: %v: %s", err, output)
		}
		args, _ := os.ReadFile(filepath.Join(out, "args"))
		if string(args) != "pr\ncreate\n--title\ntwo words\n" {
			t.Errorf("gh args = %q", args)
		}
		env, _ := os.ReadFile(filepath.Join(out, "env"))
		want := fmt.Sprintf("GH_TOKEN=ghu_app GITHUB_TOKEN= GH_CONFIG_DIR=%s/agent-github-token-gh\n", out)
		if string(env) != want {
			t.Errorf("gh environment = %q, want %q", env, want)
		}
	})

	t.Run("gh auth is disabled", func(t *testing.T) {
		out, output, err := runShim(t, "Iv23liTest", "auth", "token")
		if err == nil || !strings.Contains(output, "gh auth is disabled") {
			t.Errorf("Expected gh auth to be refused, got: %v: %s", err, output)
		}
		if _, statErr := os.Stat(filepath.Join(out, "args")); statErr == nil {
			t.Error("The real gh ran")
		}
	})

	t.Run("no token, no gh", func(t *testing.T) {
		out, output, err := runShim(t, "Iv23liOther", "pr", "list")
		if err == nil || !strings.Contains(output, "wrong App") {
			t.Errorf("Expected the helper's error, got: %v: %s", err, output)
		}
		if _, statErr := os.Stat(filepath.Join(out, "args")); statErr == nil {
			t.Error("The real gh ran without a token")
		}
	})

	t.Run("gh not installed", func(t *testing.T) {
		if !strings.Contains(ghShim(helper, "Iv23liTest", ""), "gh is not installed") {
			t.Error("Shim without a real gh does not report it")
		}
	})
}

func preToolUseInput(t *testing.T, tool, command string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       tool,
		"tool_input":      map[string]any{"command": command},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// denyReason returns the hook's deny reason, or "" if it allowed the call.
func denyReason(t *testing.T, r result) string {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("Exit code = %d, stderr %q", r.code, r.stderr)
	}
	if r.stdout == "" {
		return ""
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v: %q", err, r.stdout)
	}
	if out.HookSpecificOutput.HookEventName != "PreToolUse" || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("Unexpected hook output: %q", r.stdout)
	}
	return out.HookSpecificOutput.PermissionDecisionReason
}

func TestPreToolUse(t *testing.T) {
	hook := func(t *testing.T, service *fakeService, command string) string {
		t.Helper()
		h := newHookEnv(t)
		return denyReason(t, h.run(service, preToolUseInput(t, "Bash", command),
			"claude-hook", "pre-tool-use", "--client-id", "Iv23liTest"))
	}

	t.Run("blocks other credentials", func(t *testing.T) {
		for command, want := range map[string]string{
			"security find-generic-password -s gh:github.com -w": "Keychain",
			"/usr/bin/security -q dump-keychain":                 "Keychain",
			"echo 'find-generic-password -s x' | security -i":    "Keychain",
			"gh auth token":                          "gh auth",
			"cd repo && gh auth status --show-token": "gh auth",
			"cat ~/.ssh/id_ed25519":                  "SSH",
			"ls $HOME/.ssh":                          "SSH",
			"ssh-add -l":                             "SSH",
			"cat ~/.config/gh/hosts.yml":             "other GitHub credentials",
			"cat ~/.git-credentials":                 "other GitHub credentials",
			"cat ~/.netrc":                           "other GitHub credentials",
			"printf 'host=github.com\\n\\n' | git credential-osxkeychain get":       "credential helpers",
			"git -c credential.helper=osxkeychain push":                             "how git authenticates",
			"git config --global url.git@github.com:.insteadOf https://github.com/": "how git authenticates",
			"unset GIT_CONFIG_COUNT; git push":                                      "how git authenticates",
			"GH_TOKEN=ghp_mine gh repo list":                                        "by hand",
			"export GITHUB_TOKEN=ghp_mine":                                          "by hand",
			"env -i git push":                                                       "environment",
			"GH_CONFIG_DIR=~/.config/gh gh repo list":                               "other GitHub credentials",
		} {
			service := &fakeService{token: "ghu_app"}
			if reason := hook(t, service, command); !strings.Contains(reason, want) {
				t.Errorf("%q: reason %q, want it to mention %q", command, reason, want)
			}
		}
	})

	t.Run("blocks running the real gh directly", func(t *testing.T) {
		h := newHookEnv(t)
		r := h.run(&fakeService{token: "ghu_app"}, preToolUseInput(t, "Bash", h.realGh+" api user"),
			"claude-hook", "pre-tool-use", "--client-id", "Iv23liTest")
		if reason := denyReason(t, r); !strings.Contains(reason, "runs gh directly") {
			t.Errorf("Reason %q", reason)
		}
	})

	t.Run("allows unrelated commands without touching the token", func(t *testing.T) {
		for _, command := range []string{
			"ls -la",
			"go test ./internal/security ./...",
			`git commit -m "Document ssh setup"`,
			"git status && git diff",
			"echo security review done",
			"cat .sshrc",
		} {
			service := &fakeService{token: "ghu_app"}
			if reason := hook(t, service, command); reason != "" {
				t.Errorf("%q was blocked: %s", command, reason)
			}
			if service.verifiedCalls != 0 {
				t.Errorf("%q checked the token", command)
			}
		}
	})

	t.Run("checks the token before GitHub commands", func(t *testing.T) {
		for _, command := range []string{
			"gh pr list",
			"cd repo && gh repo view",
			"git push origin main",
			"git -C repo fetch --all",
			"git clone https://github.com/owner/repo.git",
			"(git pull)",
		} {
			service := &fakeService{token: "ghu_app"}
			if reason := hook(t, service, command); reason != "" {
				t.Errorf("%q was blocked: %s", command, reason)
			}
			if service.verifiedCalls != 1 || service.clientID != "Iv23liTest" {
				t.Errorf("%q: %d token checks with client ID %q, want 1 with Iv23liTest", command, service.verifiedCalls, service.clientID)
			}
		}
	})

	t.Run("blocks GitHub commands without a good token", func(t *testing.T) {
		for err, want := range map[error]string{
			authtoken.ErrNotLoggedIn:      "agent-github-token login",
			authtoken.ErrLoginExpired:     "agent-github-token login",
			authtoken.ErrClientIDMismatch: "not from the expected GitHub App",
			errors.New("GitHub is down"):  "GitHub is down",
		} {
			service := &fakeService{verifiedErr: err}
			if reason := hook(t, service, "gh pr list"); !strings.Contains(reason, want) {
				t.Errorf("%v: reason %q, want it to mention %q", err, reason, want)
			}
		}
	})

	t.Run("ignores other tools", func(t *testing.T) {
		h := newHookEnv(t)
		r := h.run(&fakeService{}, preToolUseInput(t, "Read", "cat ~/.ssh/id_rsa"), "claude-hook", "pre-tool-use", "--client-id", "Iv23liTest")
		r.check(t, 0, "")
	})

	t.Run("fails closed", func(t *testing.T) {
		h := newHookEnv(t)
		h.run(&fakeService{}, "not json", "claude-hook", "pre-tool-use", "--client-id", "Iv23liTest").check(t, 2, "", "cannot read the hook input")
		h.run(&fakeService{}, preToolUseInput(t, "Bash", "ls"), "claude-hook", "pre-tool-use").check(t, 2, "", "needs --client-id")
		h.run(&fakeService{}, preToolUseInput(t, "Bash", "ls"), "claude-hook", "pre-tool-use", "--client-id").check(t, 2, "", "needs a value")

		c, _, stderr := newTestCLI(&fakeService{}, preToolUseInput(t, "Bash", "gh pr list"), h.env, h.cacheDir)
		c.newService = func(string) (tokenService, error) { return nil, errors.New("only macOS is supported") }
		if code := c.run(context.Background(), []string{"claude-hook", "pre-tool-use", "--client-id", "Iv23liTest"}); code != 2 {
			t.Errorf("Exit code = %d, want 2 (stderr %q)", code, stderr.String())
		}
	})
}

func TestClaudeSettings(t *testing.T) {
	r := runCLI(&fakeService{}, "", "claude-settings", "--client-id", "Iv23liTest")
	if r.code != 0 || !strings.Contains(r.stderr, "~/.claude/settings.json") {
		t.Fatalf("code %d, stderr %q", r.code, r.stderr)
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
		Sandbox struct {
			Enabled                      bool `json:"enabled"`
			AllowUnsandboxedCommands     bool `json:"allowUnsandboxedCommands"`
			FailIfUnavailable            bool `json:"failIfUnavailable"`
			EnableWeakerNetworkIsolation bool `json:"enableWeakerNetworkIsolation"`
			Network                      struct {
				AllowedDomains []string `json:"allowedDomains"`
			} `json:"network"`
			Credentials struct {
				Files []struct {
					Path, Mode string
				} `json:"files"`
				EnvVars []struct {
					Name, Mode string
				} `json:"envVars"`
			} `json:"credentials"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &settings); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.stdout)
	}

	start, pre := settings.Hooks["SessionStart"], settings.Hooks["PreToolUse"]
	if len(start) != 1 || start[0].Hooks[0].Command != "'"+testHelperPath+"' claude-hook session-start --client-id 'Iv23liTest'" {
		t.Errorf("SessionStart hooks = %+v", start)
	}
	if len(pre) != 1 || pre[0].Matcher != "Bash" || pre[0].Hooks[0].Command != "'"+testHelperPath+"' claude-hook pre-tool-use --client-id 'Iv23liTest'" {
		t.Errorf("PreToolUse hooks = %+v", pre)
	}
	sb := settings.Sandbox
	if !sb.Enabled || sb.AllowUnsandboxedCommands || !sb.FailIfUnavailable || !sb.EnableWeakerNetworkIsolation {
		t.Errorf("Sandbox = %+v", sb)
	}
	if len(sb.Credentials.Files) != len(credentialFiles) || len(sb.Credentials.EnvVars) != len(tokenVariables) {
		t.Errorf("Credentials = %+v", sb.Credentials)
	}
	if !strings.Contains(strings.Join(settings.Permissions.Deny, " "), "Read(~/.ssh/**)") {
		t.Errorf("Permissions deny = %v", settings.Permissions.Deny)
	}

	runCLI(&fakeService{}, "", "claude-settings").check(t, 1, "", "needs --client-id")
}

func TestFindExecutable(t *testing.T) {
	dir := t.TempDir()
	shimDir, otherDir := filepath.Join(dir, "shim"), filepath.Join(dir, "other")
	writeScript(t, filepath.Join(shimDir, "gh"), "exit 0")
	writeScript(t, filepath.Join(otherDir, "gh"), "exit 0")
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := findExecutable("gh", strings.Join([]string{shimDir, dir, otherDir}, ":"), shimDir)
	if got != filepath.Join(otherDir, "gh") {
		t.Errorf("findExecutable = %q, want the one in %s", got, otherDir)
	}
	if got := findExecutable("gh", shimDir, shimDir); got != "" {
		t.Errorf("findExecutable = %q, want none", got)
	}
}

func TestShellQuote(t *testing.T) {
	for _, s := range []string{"plain", "with space", "it's", `"$HOME" $(rm -rf /) ` + "`x`", ""} {
		out, err := exec.Command("bash", "-c", "printf %s "+shellQuote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("shellQuote(%q) round trip = %q, %v", s, out, err)
		}
	}
}
