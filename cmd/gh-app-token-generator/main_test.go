package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oikarinen/agent-gh-token-generator/internal/authtoken"
)

var testNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// fakeService records calls and returns canned results.
type fakeService struct {
	loginClientID string
	loginErr      error
	token         string
	tokenErr      error
	tokenCalls    int
	status        *authtoken.Status
	statusErr     error
	logoutErr     error
	logoutCalls   int
}

func (f *fakeService) Login(ctx context.Context, clientID string, prompt func(string, string)) (*authtoken.Status, error) {
	f.loginClientID = clientID
	if f.loginErr != nil {
		return nil, f.loginErr
	}
	prompt("ABCD-1234", "https://github.com/login/device")
	return &authtoken.Status{ClientID: clientID, LoginExpiresAt: testNow.Add(180 * 24 * time.Hour)}, nil
}

func (f *fakeService) Token(ctx context.Context) (string, error) {
	f.tokenCalls++
	return f.token, f.tokenErr
}

func (f *fakeService) Status() (*authtoken.Status, error) { return f.status, f.statusErr }

func (f *fakeService) Logout() error {
	f.logoutCalls++
	return f.logoutErr
}

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(service *fakeService, stdin string, args ...string) result {
	var stdout, stderr bytes.Buffer
	c := &cli{
		stdin:      strings.NewReader(stdin),
		stdout:     &stdout,
		stderr:     &stderr,
		newService: func() (tokenService, error) { return service, nil },
		now:        func() time.Time { return testNow },
	}
	code := c.run(context.Background(), args)
	return result{code, stdout.String(), stderr.String()}
}

func (r result) check(t *testing.T, wantCode int, wantStdout string, stderrContains ...string) {
	t.Helper()
	if r.code != wantCode {
		t.Errorf("Exit code = %d, want %d (stderr: %q)", r.code, wantCode, r.stderr)
	}
	if r.stdout != wantStdout {
		t.Errorf("stdout = %q, want %q", r.stdout, wantStdout)
	}
	for _, want := range stderrContains {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", r.stderr, want)
		}
	}
}

func TestUsage(t *testing.T) {
	runCLI(&fakeService{}, "").check(t, 1, "", "Usage:")
	runCLI(&fakeService{}, "", "frobnicate").check(t, 1, "", `Unknown command "frobnicate"`, "Usage:")
	runCLI(&fakeService{}, "", "login").check(t, 1, "", "Usage:")
	runCLI(&fakeService{}, "", "token", "extra").check(t, 1, "", "Usage:")
	runCLI(&fakeService{}, "", "git-credential").check(t, 1, "", "Usage:")

	r := runCLI(&fakeService{}, "", "--help")
	if r.code != 0 || !strings.Contains(r.stdout, "Usage:") {
		t.Errorf("--help: code %d, stdout %q", r.code, r.stdout)
	}
}

func TestServiceUnavailable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	c := &cli{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		newService: func() (tokenService, error) { return nil, errors.New("only macOS is supported") },
		now:        time.Now,
	}
	if code := c.run(context.Background(), []string{"token"}); code != 1 || !strings.Contains(stderr.String(), "only macOS") {
		t.Errorf("code %d, stderr %q", code, stderr.String())
	}
}

func TestLogin(t *testing.T) {
	service := &fakeService{}
	r := runCLI(service, "", "login", "Iv23liTest")
	r.check(t, 0, "", "https://github.com/login/device", "ABCD-1234", "Logged in", "2027-03-31")
	if service.loginClientID != "Iv23liTest" {
		t.Errorf("Login called with %q", service.loginClientID)
	}

	runCLI(&fakeService{loginErr: errors.New("access_denied")}, "", "login", "Iv23liTest").check(t, 1, "", "Error: access_denied")
}

func TestToken(t *testing.T) {
	runCLI(&fakeService{token: "ghu_abc"}, "", "token").check(t, 0, "ghu_abc")
	runCLI(&fakeService{tokenErr: authtoken.ErrNotLoggedIn}, "", "token").check(t, 1, "", "not logged in", "agent-github-token login")
	runCLI(&fakeService{tokenErr: fmt.Errorf("%w: bad_refresh_token", authtoken.ErrLoginExpired)}, "", "token").check(t, 1, "", "login expired", "agent-github-token login")
	runCLI(&fakeService{tokenErr: errors.New("network down")}, "", "token").check(t, 1, "", "Error: network down")
}

func TestStatus(t *testing.T) {
	t.Run("logged in", func(t *testing.T) {
		service := &fakeService{status: &authtoken.Status{
			ClientID:             "Iv23liTest",
			AccessTokenExpiresAt: testNow.Add(7*time.Hour + 59*time.Minute),
			LoginExpiresAt:       testNow.Add(181 * 24 * time.Hour),
		}}
		r := runCLI(service, "", "status")
		if r.code != 0 {
			t.Errorf("Exit code = %d", r.code)
		}
		for _, want := range []string{"Iv23liTest", "in 7h 59m", "renews automatically", "in 181 days"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("stdout = %q, want it to contain %q", r.stdout, want)
			}
		}
	})

	t.Run("access token expired", func(t *testing.T) {
		service := &fakeService{status: &authtoken.Status{
			AccessTokenExpiresAt: testNow.Add(-time.Hour),
			LoginExpiresAt:       testNow.Add(30 * 24 * time.Hour),
		}}
		r := runCLI(service, "", "status")
		if r.code != 0 || !strings.Contains(r.stdout, "renews on next use") {
			t.Errorf("code %d, stdout %q", r.code, r.stdout)
		}
	})

	t.Run("login expired", func(t *testing.T) {
		service := &fakeService{status: &authtoken.Status{
			AccessTokenExpiresAt: testNow.Add(-time.Hour),
			LoginExpiresAt:       testNow.Add(-time.Minute),
		}}
		r := runCLI(service, "", "status")
		if r.code != 1 || !strings.Contains(r.stdout, "expired on") || !strings.Contains(r.stdout, "agent-github-token login") {
			t.Errorf("code %d, stdout %q", r.code, r.stdout)
		}
	})

	t.Run("not logged in", func(t *testing.T) {
		runCLI(&fakeService{statusErr: authtoken.ErrNotLoggedIn}, "", "status").check(t, 1, "", "not logged in")
	})
}

func TestLogout(t *testing.T) {
	runCLI(&fakeService{}, "", "logout").check(t, 0, "", "Removed the stored tokens", "settings/apps/authorizations")
	runCLI(&fakeService{logoutErr: authtoken.ErrNotLoggedIn}, "", "logout").check(t, 0, "", "No stored tokens")
	runCLI(&fakeService{logoutErr: errors.New("keychain is locked")}, "", "logout").check(t, 1, "", "keychain is locked")
}

func TestGitCredential(t *testing.T) {
	githubRequest := "protocol=https\nhost=github.com\npath=org/repo.git\n\n"

	t.Run("answers get for https://github.com", func(t *testing.T) {
		runCLI(&fakeService{token: "ghu_abc"}, githubRequest, "git-credential", "get").
			check(t, 0, "username=x-access-token\npassword=ghu_abc\n")
	})

	for name, request := range map[string]string{
		"other host":      "protocol=https\nhost=gitlab.com\n\n",
		"plain http":      "protocol=http\nhost=github.com\n\n",
		"empty request":   "",
		"subdomain":       "protocol=https\nhost=gist.github.com\n\n",
		"no blank ending": "protocol=https\nhost=example.com",
	} {
		t.Run("ignores "+name, func(t *testing.T) {
			service := &fakeService{token: "ghu_abc"}
			runCLI(service, request, "git-credential", "get").check(t, 0, "")
			if service.tokenCalls != 0 {
				t.Error("A token was requested")
			}
		})
	}

	for _, operation := range []string{"store", "erase"} {
		t.Run("ignores "+operation, func(t *testing.T) {
			service := &fakeService{token: "ghu_abc"}
			runCLI(service, githubRequest+"username=x\npassword=y\n", "git-credential", operation).check(t, 0, "")
			if service.tokenCalls != 0 {
				t.Error("A token was requested")
			}
		})
	}

	t.Run("reports token errors", func(t *testing.T) {
		runCLI(&fakeService{tokenErr: authtoken.ErrLoginExpired}, githubRequest, "git-credential", "get").
			check(t, 1, "", "agent-github-token login")
	})
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:               "0h 1m",
		7*time.Hour + 59*time.Minute:   "7h 59m",
		47*time.Hour + 30*time.Minute:  "47h 30m",
		48 * time.Hour:                 "2 days",
		181*24*time.Hour + 5*time.Hour: "181 days",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
