// Package authtoken gets GitHub App user access tokens through GitHub's
// device flow, keeps them in the macOS Keychain, and renews the short-lived
// access token with the refresh token when it is about to expire.
//
// The user authorizes once in the browser. After that, access tokens (valid
// for 8 hours) are renewed automatically. Each renewal also replaces the
// refresh token (valid for 6 months), so the browser step only has to be
// repeated if the tool goes unused for 6 months or the authorization is revoked.
package authtoken

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// refreshMargin is how long before expiry an access token is renewed, so a
// token handed out has time to finish the command it is used for.
const refreshMargin = 10 * time.Minute

var (
	// ErrNotLoggedIn means no tokens are stored.
	ErrNotLoggedIn = errors.New("not logged in")
	// ErrLoginExpired means the refresh token expired or was revoked.
	ErrLoginExpired = errors.New("login expired")
	// ErrExpirationDisabled means the App issues user tokens that never
	// expire, so there is no refresh token to renew them with.
	ErrExpirationDisabled = errors.New("the GitHub App issues user tokens that never expire; turn on " +
		"\"User-to-server token expiration\" under the App's Optional features and log in again")
	// ErrClientIDMismatch means the stored tokens belong to a different App
	// than the one expected.
	ErrClientIDMismatch = errors.New("the stored login is for a different GitHub App")
)

// clientIDPattern matches GitHub App client IDs such as "Iv23liAbCdEf0123".
var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Tokens are the credentials kept between runs.
type Tokens struct {
	ClientID              string    `json:"client_id"`
	AccessToken           string    `json:"access_token"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshToken          string    `json:"refresh_token"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
}

// Status describes the stored login without exposing the tokens.
type Status struct {
	ClientID             string
	AccessTokenExpiresAt time.Time
	LoginExpiresAt       time.Time // when the refresh token expires
}

// Store keeps Tokens between runs.
type Store interface {
	// Load returns ErrNotLoggedIn if nothing is stored.
	Load() (*Tokens, error)
	Save(*Tokens) error
	// Delete returns ErrNotLoggedIn if nothing is stored.
	Delete() error
}

// Manager runs the device flow and hands out access tokens, renewing them
// when needed.
type Manager struct {
	GitHub   *GitHub
	Store    Store
	LockPath string
	// ClientID, if set, is the only GitHub App whose tokens are handed out.
	ClientID string
	// VerifiedPath records the hash of the last access token GitHub
	// confirmed belongs to the App, so each token is checked only once.
	VerifiedPath string
	Now          func() time.Time
	// Wait pauses between device flow polls. It returns early with the
	// context's error if the context ends.
	Wait func(ctx context.Context, d time.Duration) error
}

// New returns a Manager that uses github.com and the macOS Keychain.
func New() (*Manager, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("only macOS is supported: tokens are stored in the macOS Keychain")
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	return &Manager{
		GitHub: &GitHub{
			BaseURL:    "https://github.com",
			APIBaseURL: "https://api.github.com",
			Client:     &http.Client{Timeout: 30 * time.Second},
		},
		Store:        NewKeychainStore(),
		LockPath:     filepath.Join(cacheDir, "agent-github-token", "token.lock"),
		VerifiedPath: filepath.Join(cacheDir, "agent-github-token", "verified-token"),
		Now:          time.Now,
		Wait:         wait,
	}, nil
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Login runs the device flow for the GitHub App with clientID and stores the
// resulting tokens. prompt is called with the code the user has to enter at
// verificationURI.
func (m *Manager) Login(ctx context.Context, clientID string, prompt func(userCode, verificationURI string)) (*Status, error) {
	if !clientIDPattern.MatchString(clientID) {
		return nil, fmt.Errorf("invalid client ID %q: use the Client ID from the GitHub App's settings page (not the App ID)", clientID)
	}

	dc, err := m.GitHub.RequestDeviceCode(ctx, clientID)
	if err != nil {
		return nil, fmt.Errorf("starting the device flow: %w", explainOAuthError(err))
	}
	prompt(dc.UserCode, dc.VerificationURI)

	expiresIn := time.Duration(dc.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, expiresIn)
	defer cancel()

	interval := time.Duration(dc.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	for {
		if err := m.Wait(ctx, interval); err != nil {
			return nil, fmt.Errorf("the code was not entered in time; run login again: %w", err)
		}
		requestedAt := m.Now()
		resp, err := m.GitHub.PollDeviceToken(ctx, clientID, dc.DeviceCode)
		var oauthErr *OAuthError
		switch {
		case err == nil:
			tokens, err := newTokens(clientID, resp, requestedAt)
			if err != nil {
				return nil, err
			}
			// Have GitHub confirm the token belongs to this App and that the
			// App is installed somewhere, before anything relies on it.
			if err := m.verify(ctx, tokens, clientID, true); err != nil {
				return nil, err
			}
			if err := m.save(tokens); err != nil {
				return nil, err
			}
			return tokens.status(), nil
		case errors.As(err, &oauthErr) && oauthErr.Code == "authorization_pending":
			continue
		case errors.As(err, &oauthErr) && oauthErr.Code == "slow_down":
			if oauthErr.Interval > 0 {
				interval = time.Duration(oauthErr.Interval) * time.Second
			} else {
				interval += 5 * time.Second
			}
		default:
			return nil, fmt.Errorf("waiting for authorization: %w", explainOAuthError(err))
		}
	}
}

// Token returns a valid access token, renewing it first if it expires within
// refreshMargin. If m.ClientID is set, it refuses tokens of any other App.
func (m *Manager) Token(ctx context.Context) (string, error) {
	tokens, err := m.tokens(ctx)
	if err != nil {
		return "", err
	}
	return tokens.AccessToken, nil
}

// VerifiedToken is like Token, but also has GitHub confirm that the token
// belongs to the App with m.ClientID (or, if unset, the stored client ID).
// GitHub is asked once per access token; the answer is remembered by hash.
func (m *Manager) VerifiedToken(ctx context.Context) (string, error) {
	tokens, err := m.tokens(ctx)
	if err != nil {
		return "", err
	}
	clientID := m.ClientID
	if clientID == "" {
		clientID = tokens.ClientID
	}
	if !m.verified(tokens.AccessToken) {
		if err := m.verify(ctx, tokens, clientID, false); err != nil {
			return "", err
		}
	}
	return tokens.AccessToken, nil
}

func (m *Manager) tokens(ctx context.Context) (*Tokens, error) {
	tokens, err := m.load()
	if err != nil {
		return nil, err
	}
	if m.fresh(tokens) {
		return tokens, nil
	}

	defer m.lock()()

	// Another process may have renewed the token while we waited for the lock.
	tokens, err = m.load()
	if err != nil {
		return nil, err
	}
	if m.fresh(tokens) {
		return tokens, nil
	}
	if !m.Now().Before(tokens.RefreshTokenExpiresAt) {
		return nil, ErrLoginExpired
	}

	requestedAt := m.Now()
	resp, err := m.GitHub.RefreshToken(ctx, tokens.ClientID, tokens.RefreshToken)
	if err != nil {
		var oauthErr *OAuthError
		if errors.As(err, &oauthErr) && oauthErr.Code == "bad_refresh_token" {
			// If another process renewed first (possible when the lock is
			// unavailable), our refresh token is spent but its tokens are stored.
			if current, loadErr := m.load(); loadErr == nil &&
				current.RefreshToken != tokens.RefreshToken && m.fresh(current) {
				return current, nil
			}
			return nil, fmt.Errorf("%w: %v", ErrLoginExpired, err)
		}
		return nil, fmt.Errorf("renewing the access token: %w", err)
	}
	// GitHub only accepts the refresh token together with the client ID it
	// was issued for, so the renewed tokens belong to the same App.
	renewed, err := newTokens(tokens.ClientID, resp, requestedAt)
	if err != nil {
		return nil, err
	}
	if err := m.Store.Save(renewed); err != nil {
		// GitHub has already invalidated the old tokens, so a new login is needed.
		return nil, fmt.Errorf("storing the renewed tokens failed; run login again: %w", err)
	}
	return renewed, nil
}

// load reads the stored tokens and checks they belong to m.ClientID, if set.
func (m *Manager) load() (*Tokens, error) {
	tokens, err := m.Store.Load()
	if err != nil {
		return nil, err
	}
	if m.ClientID != "" && tokens.ClientID != m.ClientID {
		return nil, fmt.Errorf("%w: it is for client ID %q, but %q is expected; run login again",
			ErrClientIDMismatch, tokens.ClientID, m.ClientID)
	}
	return tokens, nil
}

// verify asks GitHub which App the access token belongs to. GitHub lists only
// installations of the App that issued the token, so every installation must
// carry clientID. At login, requireInstallation also insists on at least one,
// since a token for an App that is installed nowhere can't reach anything.
func (m *Manager) verify(ctx context.Context, tokens *Tokens, clientID string, requireInstallation bool) error {
	installations, err := m.GitHub.UserInstallations(ctx, tokens.AccessToken)
	if err != nil {
		return fmt.Errorf("checking the token with GitHub: %w", err)
	}
	if requireInstallation && len(installations) == 0 {
		return errors.New("the GitHub App is not installed on any account you can access; " +
			"install it on the repositories the agent may use, then log in again")
	}
	for _, installation := range installations {
		if installation.ClientID != clientID {
			return fmt.Errorf("%w: GitHub reports the token belongs to App %q (client ID %q), not client ID %q",
				ErrClientIDMismatch, installation.AppSlug, installation.ClientID, clientID)
		}
	}
	m.markVerified(tokens.AccessToken)
	return nil
}

// verified reports whether GitHub already confirmed accessToken.
func (m *Manager) verified(accessToken string) bool {
	if m.VerifiedPath == "" {
		return false
	}
	data, err := os.ReadFile(m.VerifiedPath)
	return err == nil && strings.TrimSpace(string(data)) == tokenHash(accessToken)
}

// markVerified remembers that GitHub confirmed accessToken. It is best
// effort: if the file can't be written, the token is simply checked again.
func (m *Manager) markVerified(accessToken string) {
	if m.VerifiedPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.VerifiedPath), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(m.VerifiedPath, []byte(tokenHash(accessToken)+"\n"), 0o600)
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Status describes the stored login.
func (m *Manager) Status() (*Status, error) {
	tokens, err := m.Store.Load()
	if err != nil {
		return nil, err
	}
	return tokens.status(), nil
}

// Logout removes the stored tokens. It does not revoke the authorization on
// GitHub, which needs the App's client secret.
func (m *Manager) Logout() error {
	defer m.lock()()
	return m.Store.Delete()
}

func (m *Manager) save(tokens *Tokens) error {
	defer m.lock()()
	return m.Store.Save(tokens)
}

// lock takes the refresh lock and returns the function that releases it. The
// lock is best effort: where the lock file cannot be created (for example in
// a sandbox that only allows writes to the working directory), it carries on
// without it rather than failing.
func (m *Manager) lock() (unlock func()) {
	unlock, err := lockFile(m.LockPath)
	if err != nil {
		return func() {}
	}
	return unlock
}

func (m *Manager) fresh(tokens *Tokens) bool {
	return m.Now().Add(refreshMargin).Before(tokens.AccessTokenExpiresAt)
}

func newTokens(clientID string, resp *tokenResponse, requestedAt time.Time) (*Tokens, error) {
	if resp.RefreshToken == "" || resp.ExpiresIn <= 0 || resp.RefreshTokenExpiresIn <= 0 {
		return nil, ErrExpirationDisabled
	}
	return &Tokens{
		ClientID:              clientID,
		AccessToken:           resp.AccessToken,
		AccessTokenExpiresAt:  requestedAt.Add(time.Duration(resp.ExpiresIn) * time.Second),
		RefreshToken:          resp.RefreshToken,
		RefreshTokenExpiresAt: requestedAt.Add(time.Duration(resp.RefreshTokenExpiresIn) * time.Second),
	}, nil
}

func (t *Tokens) status() *Status {
	return &Status{
		ClientID:             t.ClientID,
		AccessTokenExpiresAt: t.AccessTokenExpiresAt,
		LoginExpiresAt:       t.RefreshTokenExpiresAt,
	}
}

// explainOAuthError adds a hint for errors caused by the App's configuration.
func explainOAuthError(err error) error {
	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) {
		return err
	}
	switch oauthErr.Code {
	case "device_flow_disabled":
		return fmt.Errorf("%w (turn on \"Enable Device Flow\" in the GitHub App's settings)", err)
	case "incorrect_client_credentials":
		return fmt.Errorf("%w (use the Client ID from the GitHub App's settings page, not the App ID)", err)
	case "access_denied":
		return fmt.Errorf("%w (the authorization request was cancelled)", err)
	case "expired_token":
		return fmt.Errorf("%w (the code expired before it was entered; run login again)", err)
	}
	return err
}
