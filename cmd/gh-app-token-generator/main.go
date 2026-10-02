package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/oikarinen/agent-gh-token-generator/internal/authtoken"
)

const usage = `Usage: gh-app-token-generator <command> [--client-id ID]

Commands:
  login <client-id>          Authorize with GitHub's device flow and store the tokens in the Keychain
  token                      Print a valid access token, renewing it first if needed
  status                     Show when the stored tokens expire
  logout                     Remove the stored tokens from the Keychain
  git-credential get         Act as a git credential helper for https://github.com
  claude-hook <event>        Run a Claude Code hook (session-start or pre-tool-use); needs --client-id
  claude-settings            Print Claude Code settings that install the hooks; needs --client-id

With --client-id, only tokens issued for that GitHub App are handed out.
`

// loginHint is shown when a new device flow login is needed.
const loginHint = "run 'agent-github-token login' to authorize in the browser"

// tokenService is the part of authtoken.Manager the commands use.
type tokenService interface {
	Login(ctx context.Context, clientID string, prompt func(userCode, verificationURI string)) (*authtoken.Status, error)
	Token(ctx context.Context) (string, error)
	VerifiedToken(ctx context.Context) (string, error)
	Status() (*authtoken.Status, error)
	Logout() error
}

type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	// newService returns the token service; with a non-empty clientID it
	// only hands out tokens issued for that GitHub App.
	newService func(clientID string) (tokenService, error)
	now        func() time.Time
	getenv     func(string) string
	executable func() (string, error)
	cacheDir   func() (string, error)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	c := &cli{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		newService: func(clientID string) (tokenService, error) {
			m, err := authtoken.New()
			if err != nil {
				return nil, err
			}
			m.ClientID = clientID
			return m, nil
		},
		now:        time.Now,
		getenv:     os.Getenv,
		executable: os.Executable,
		cacheDir:   os.UserCacheDir,
	}
	code := c.run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// run executes the command in args (without the program name) and returns
// the exit code.
func (c *cli) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, usage)
		return 1
	}
	command := args[0]
	clientID, args, err := parseArgs(args[1:])
	if err != nil {
		fmt.Fprintf(c.stderr, "%v\n\n%s", err, usage)
		if command == "claude-hook" {
			return 2 // a misconfigured hook blocks rather than letting commands through
		}
		return 1
	}

	wantArgs := map[string]int{"login": 1, "token": 0, "status": 0, "logout": 0, "git-credential": 1, "claude-hook": 1, "claude-settings": 0}
	switch n, known := wantArgs[command]; {
	case command == "help" || command == "-h" || command == "--help":
		fmt.Fprint(c.stdout, usage)
		return 0
	case !known:
		fmt.Fprintf(c.stderr, "Unknown command %q\n\n%s", command, usage)
		return 1
	case len(args) != n:
		fmt.Fprint(c.stderr, usage)
		return 1
	}

	// The Claude Code commands create the token service only when needed.
	switch command {
	case "claude-hook":
		return c.claudeHook(ctx, args[0], clientID)
	case "claude-settings":
		if clientID == "" {
			return c.fail(errors.New("claude-settings needs --client-id"))
		}
		return c.claudeSettings(clientID)
	}

	service, err := c.newService(clientID)
	if err != nil {
		return c.fail(err)
	}

	switch command {
	case "login":
		return c.login(ctx, service, args[0])
	case "token":
		return c.token(ctx, service)
	case "status":
		return c.status(service)
	case "logout":
		return c.logout(service)
	default:
		return c.gitCredential(ctx, service, args[0])
	}
}

// parseArgs separates the --client-id option from positional arguments.
func parseArgs(args []string) (clientID string, positional []string, err error) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--client-id":
			if i+1 >= len(args) || args[i+1] == "" {
				return "", nil, errors.New("--client-id needs a value")
			}
			clientID = args[i+1]
			i++
		case strings.HasPrefix(arg, "--client-id="):
			clientID = strings.TrimPrefix(arg, "--client-id=")
			if clientID == "" {
				return "", nil, errors.New("--client-id needs a value")
			}
		default:
			positional = append(positional, arg)
		}
	}
	return clientID, positional, nil
}

func (c *cli) login(ctx context.Context, service tokenService, clientID string) int {
	status, err := service.Login(ctx, clientID, func(userCode, verificationURI string) {
		fmt.Fprintf(c.stderr, "Open %s in your browser and enter the code: %s\n", verificationURI, userCode)
		fmt.Fprintln(c.stderr, "Waiting for authorization...")
	})
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintln(c.stderr, "Logged in. Tokens are stored in the macOS Keychain and renew automatically.")
	fmt.Fprintf(c.stderr, "If unused, the login expires on %s.\n", formatTime(status.LoginExpiresAt))
	return 0
}

func (c *cli) token(ctx context.Context, service tokenService) int {
	token, err := service.Token(ctx)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprint(c.stdout, token)
	return 0
}

func (c *cli) status(service tokenService) int {
	status, err := service.Status()
	if err != nil {
		return c.fail(err)
	}
	now := c.now()
	fmt.Fprintf(c.stdout, "Logged in with GitHub App client ID %s.\n", status.ClientID)
	if now.Before(status.AccessTokenExpiresAt) {
		fmt.Fprintf(c.stdout, "Access token: expires %s (in %s); renews automatically\n",
			formatTime(status.AccessTokenExpiresAt), formatDuration(status.AccessTokenExpiresAt.Sub(now)))
	} else {
		fmt.Fprintln(c.stdout, "Access token: expired; renews on next use")
	}
	if !now.Before(status.LoginExpiresAt) {
		fmt.Fprintf(c.stdout, "Login:        expired on %s; %s\n", formatTime(status.LoginExpiresAt), loginHint)
		return 1
	}
	fmt.Fprintf(c.stdout, "Login:        expires %s (in %s) unless used before then\n",
		formatTime(status.LoginExpiresAt), formatDuration(status.LoginExpiresAt.Sub(now)))
	return 0
}

func (c *cli) logout(service tokenService) int {
	if err := service.Logout(); err != nil {
		if errors.Is(err, authtoken.ErrNotLoggedIn) {
			fmt.Fprintln(c.stderr, "No stored tokens.")
			return 0
		}
		return c.fail(err)
	}
	fmt.Fprintln(c.stderr, "Removed the stored tokens from the Keychain.")
	fmt.Fprintln(c.stderr, "To also revoke the App's access, visit https://github.com/settings/apps/authorizations")
	return 0
}

// gitCredential implements the git credential helper protocol
// (https://git-scm.com/docs/gitcredentials). It answers "get" requests for
// https://github.com and ignores everything else, so git falls back to other
// helpers for other hosts.
func (c *cli) gitCredential(ctx context.Context, service tokenService, operation string) int {
	attrs := map[string]string{}
	scanner := bufio.NewScanner(c.stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			attrs[key] = value
		}
	}

	if operation != "get" || attrs["protocol"] != "https" || attrs["host"] != "github.com" {
		return 0
	}
	token, err := service.Token(ctx)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.stdout, "username=x-access-token\npassword=%s\n", token)
	return 0
}

func (c *cli) fail(err error) int {
	if errors.Is(err, authtoken.ErrNotLoggedIn) || errors.Is(err, authtoken.ErrLoginExpired) {
		fmt.Fprintf(c.stderr, "Error: %v; %s\n", err, loginHint)
	} else {
		fmt.Fprintf(c.stderr, "Error: %v\n", err)
	}
	return 1
}

func formatTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04 MST")
}

func formatDuration(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	d = d.Round(time.Minute)
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}
