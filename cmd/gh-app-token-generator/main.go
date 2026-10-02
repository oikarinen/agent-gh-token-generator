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

const usage = `Usage: gh-app-token-generator <command>

Commands:
  login <client-id>   Authorize with GitHub's device flow and store the tokens in the Keychain
  token               Print a valid access token, renewing it first if needed
  status              Show when the stored tokens expire
  logout              Remove the stored tokens from the Keychain
  git-credential get  Act as a git credential helper for https://github.com
`

// loginHint is shown when a new device flow login is needed.
const loginHint = "run 'agent-github-token login' to authorize in the browser"

// tokenService is the part of authtoken.Manager the commands use.
type tokenService interface {
	Login(ctx context.Context, clientID string, prompt func(userCode, verificationURI string)) (*authtoken.Status, error)
	Token(ctx context.Context) (string, error)
	Status() (*authtoken.Status, error)
	Logout() error
}

type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	newService     func() (tokenService, error)
	now            func() time.Time
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	c := &cli{
		stdin:      os.Stdin,
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		newService: func() (tokenService, error) { return authtoken.New() },
		now:        time.Now,
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
	command, args := args[0], args[1:]

	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(c.stdout, usage)
		return 0
	case "login":
		if len(args) != 1 {
			fmt.Fprint(c.stderr, usage)
			return 1
		}
	case "token", "status", "logout":
		if len(args) != 0 {
			fmt.Fprint(c.stderr, usage)
			return 1
		}
	case "git-credential":
		if len(args) != 1 {
			fmt.Fprint(c.stderr, usage)
			return 1
		}
	default:
		fmt.Fprintf(c.stderr, "Unknown command %q\n\n%s", command, usage)
		return 1
	}

	service, err := c.newService()
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
