package authtoken

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// memStore is an in-memory Store.
type memStore struct {
	mu     sync.Mutex
	tokens *Tokens
	saves  int
}

func (s *memStore) Load() (*Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		return nil, ErrNotLoggedIn
	}
	tokens := *s.tokens
	return &tokens, nil
}

func (s *memStore) Save(t *Tokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens := *t
	s.tokens = &tokens
	s.saves++
	return nil
}

func (s *memStore) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		return ErrNotLoggedIn
	}
	s.tokens = nil
	return nil
}

// fakeGitHub serves scripted responses for the OAuth endpoints.
type fakeGitHub struct {
	t            *testing.T
	mu           sync.Mutex
	deviceCode   any           // response to starting the device flow
	polls        []any         // responses to device token polls, in order
	refresh      any           // response to refresh requests
	refreshDelay time.Duration // how long a refresh takes
	onRefresh    func()        // runs while a refresh request is in flight
	requests     int
	refreshForms []url.Values
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("Failed to parse form: %v", err)
	}
	f.mu.Lock()
	f.requests++
	var body any
	var delay time.Duration
	var hook func()
	switch {
	case r.URL.Path == "/login/device/code":
		body = f.deviceCode
	case r.PostForm.Get("grant_type") == "refresh_token":
		f.refreshForms = append(f.refreshForms, r.PostForm)
		body, delay, hook = f.refresh, f.refreshDelay, f.onRefresh
	case len(f.polls) == 0:
		f.t.Error("Unexpected device token poll")
		body = map[string]any{"error": "expired_token"}
	default:
		body, f.polls = f.polls[0], f.polls[1:]
	}
	f.mu.Unlock()

	time.Sleep(delay)
	if hook != nil {
		hook()
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		f.t.Errorf("Failed to encode response: %v", err)
	}
}

var (
	deviceCodeOK = map[string]any{
		"device_code":      "dev123",
		"user_code":        "ABCD-1234",
		"verification_uri": "https://github.com/login/device",
		"expires_in":       900,
		"interval":         5,
	}
	tokensOK = map[string]any{
		"access_token":             "ghu_new",
		"expires_in":               28800,
		"refresh_token":            "ghr_new",
		"refresh_token_expires_in": 15897600,
	}
	pending = map[string]any{"error": "authorization_pending"}
)

type testEnv struct {
	manager *Manager
	store   *memStore
	github  *fakeGitHub
	waits   []time.Duration
}

func newTestEnv(t *testing.T) *testEnv {
	env := &testEnv{store: &memStore{}, github: &fakeGitHub{t: t}}
	env.manager = &Manager{
		GitHub:   &GitHub{BaseURL: "https://github.example", Client: handlerClient(env.github.ServeHTTP)},
		Store:    env.store,
		LockPath: filepath.Join(t.TempDir(), "token.lock"),
		Now:      func() time.Time { return testNow },
		Wait: func(ctx context.Context, d time.Duration) error {
			env.waits = append(env.waits, d)
			return ctx.Err()
		},
	}
	return env
}

// storedTokens returns tokens whose access token expires after accessLeft.
func storedTokens(accessLeft time.Duration) *Tokens {
	return &Tokens{
		ClientID:              "Iv23liTest",
		AccessToken:           "ghu_old",
		AccessTokenExpiresAt:  testNow.Add(accessLeft),
		RefreshToken:          "ghr_old",
		RefreshTokenExpiresAt: testNow.Add(90 * 24 * time.Hour),
	}
}

func TestLogin(t *testing.T) {
	t.Run("polls until authorized and stores the tokens", func(t *testing.T) {
		env := newTestEnv(t)
		env.github.deviceCode = deviceCodeOK
		env.github.polls = []any{pending, map[string]any{"error": "slow_down", "interval": 10}, tokensOK}
		var gotCode, gotURI string

		status, err := env.manager.Login(context.Background(), "Iv23liTest", func(userCode, verificationURI string) {
			gotCode, gotURI = userCode, verificationURI
		})
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}

		if gotCode != "ABCD-1234" || gotURI != "https://github.com/login/device" {
			t.Errorf("Prompted with (%q, %q)", gotCode, gotURI)
		}
		wantWaits := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}
		if len(env.waits) != len(wantWaits) {
			t.Fatalf("Waits = %v, want %v", env.waits, wantWaits)
		}
		for i := range wantWaits {
			if env.waits[i] != wantWaits[i] {
				t.Errorf("Waits = %v, want %v", env.waits, wantWaits)
				break
			}
		}
		want := Tokens{
			ClientID:              "Iv23liTest",
			AccessToken:           "ghu_new",
			AccessTokenExpiresAt:  testNow.Add(8 * time.Hour),
			RefreshToken:          "ghr_new",
			RefreshTokenExpiresAt: testNow.Add(15897600 * time.Second),
		}
		if env.store.tokens == nil || *env.store.tokens != want {
			t.Errorf("Stored tokens = %+v, want %+v", env.store.tokens, want)
		}
		if status.LoginExpiresAt != want.RefreshTokenExpiresAt || status.AccessTokenExpiresAt != want.AccessTokenExpiresAt {
			t.Errorf("Status = %+v", status)
		}
	})

	t.Run("slow_down without an interval adds 5 seconds", func(t *testing.T) {
		env := newTestEnv(t)
		env.github.deviceCode = deviceCodeOK
		env.github.polls = []any{map[string]any{"error": "slow_down"}, tokensOK}

		if _, err := env.manager.Login(context.Background(), "Iv23liTest", func(string, string) {}); err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		if len(env.waits) != 2 || env.waits[1] != 10*time.Second {
			t.Errorf("Waits = %v, want [5s 10s]", env.waits)
		}
	})

	for _, tt := range []struct {
		name     string
		response any
		wantText string
	}{
		{"access denied", map[string]any{"error": "access_denied"}, "cancelled"},
		{"code expired", map[string]any{"error": "expired_token"}, "run login again"},
		{"token expiration disabled", map[string]any{"access_token": "ghu_forever", "token_type": "bearer"}, "never expire"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.github.deviceCode = deviceCodeOK
			env.github.polls = []any{tt.response}

			_, err := env.manager.Login(context.Background(), "Iv23liTest", func(string, string) {})
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("Expected error containing %q, got: %v", tt.wantText, err)
			}
			if env.store.tokens != nil {
				t.Error("Tokens were stored after a failed login")
			}
		})
	}

	t.Run("device flow disabled in the App settings", func(t *testing.T) {
		env := newTestEnv(t)
		env.github.deviceCode = map[string]any{"error": "device_flow_disabled"}

		_, err := env.manager.Login(context.Background(), "Iv23liTest", func(string, string) {
			t.Error("Prompted despite the error")
		})
		if err == nil || !strings.Contains(err.Error(), "Enable Device Flow") {
			t.Fatalf("Expected a hint about enabling Device Flow, got: %v", err)
		}
	})

	t.Run("gives up when the code expires", func(t *testing.T) {
		env := newTestEnv(t)
		env.github.deviceCode = deviceCodeOK
		env.github.polls = []any{pending}
		calls := 0
		env.manager.Wait = func(ctx context.Context, d time.Duration) error {
			calls++
			if calls > 1 {
				return context.DeadlineExceeded
			}
			return nil
		}

		_, err := env.manager.Login(context.Background(), "Iv23liTest", func(string, string) {})
		if err == nil || !strings.Contains(err.Error(), "not entered in time") {
			t.Fatalf("Expected a timeout error, got: %v", err)
		}
	})

	for _, clientID := range []string{"", "<Your-Client-ID>", "Iv23 li", "Iv23li&x=1"} {
		t.Run("rejects client ID "+clientID, func(t *testing.T) {
			env := newTestEnv(t)

			if _, err := env.manager.Login(context.Background(), clientID, func(string, string) {}); err == nil {
				t.Fatal("Expected an error, got nil")
			}
			if env.github.requests != 0 {
				t.Error("A request was sent for an invalid client ID")
			}
		})
	}
}

func TestToken(t *testing.T) {
	t.Run("returns the stored token while it is fresh", func(t *testing.T) {
		env := newTestEnv(t)
		env.store.tokens = storedTokens(2 * time.Hour)

		token, err := env.manager.Token(context.Background())
		if err != nil || token != "ghu_old" {
			t.Fatalf("Token() = %q, %v; want ghu_old", token, err)
		}
		if env.github.requests != 0 {
			t.Error("GitHub was called for a fresh token")
		}
	})

	for _, tt := range []struct {
		name       string
		accessLeft time.Duration
	}{
		{"renews within the refresh margin", 5 * time.Minute},
		{"renews an expired token", -time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.store.tokens = storedTokens(tt.accessLeft)
			env.github.refresh = tokensOK

			token, err := env.manager.Token(context.Background())
			if err != nil || token != "ghu_new" {
				t.Fatalf("Token() = %q, %v; want ghu_new", token, err)
			}
			if len(env.github.refreshForms) != 1 {
				t.Fatalf("Refresh requests = %d, want 1", len(env.github.refreshForms))
			}
			form := env.github.refreshForms[0]
			if form.Get("refresh_token") != "ghr_old" || form.Get("client_id") != "Iv23liTest" {
				t.Errorf("Refresh form = %v", form)
			}
			stored := env.store.tokens
			if stored.AccessToken != "ghu_new" || stored.RefreshToken != "ghr_new" ||
				stored.AccessTokenExpiresAt != testNow.Add(8*time.Hour) || stored.ClientID != "Iv23liTest" {
				t.Errorf("Stored tokens = %+v", stored)
			}
		})
	}

	t.Run("refresh token expired", func(t *testing.T) {
		env := newTestEnv(t)
		env.store.tokens = storedTokens(-time.Hour)
		env.store.tokens.RefreshTokenExpiresAt = testNow.Add(-time.Minute)

		_, err := env.manager.Token(context.Background())
		if !errors.Is(err, ErrLoginExpired) {
			t.Fatalf("Expected ErrLoginExpired, got: %v", err)
		}
		if env.github.requests != 0 {
			t.Error("GitHub was called with an expired refresh token")
		}
	})

	t.Run("refresh token revoked", func(t *testing.T) {
		env := newTestEnv(t)
		env.store.tokens = storedTokens(-time.Hour)
		env.github.refresh = map[string]any{"error": "bad_refresh_token"}

		_, err := env.manager.Token(context.Background())
		if !errors.Is(err, ErrLoginExpired) {
			t.Fatalf("Expected ErrLoginExpired, got: %v", err)
		}
	})

	t.Run("uses the tokens of a process that renewed first", func(t *testing.T) {
		env := newTestEnv(t)
		env.store.tokens = storedTokens(-time.Hour)
		env.github.refresh = map[string]any{"error": "bad_refresh_token"}
		env.github.onRefresh = func() {
			// Another process renews and stores new tokens while our request
			// is in flight, which spends our refresh token.
			renewed := storedTokens(8 * time.Hour)
			renewed.AccessToken, renewed.RefreshToken = "ghu_other", "ghr_other"
			if err := env.store.Save(renewed); err != nil {
				t.Error(err)
			}
		}

		token, err := env.manager.Token(context.Background())
		if err != nil || token != "ghu_other" {
			t.Fatalf("Token() = %q, %v; want ghu_other", token, err)
		}
	})

	t.Run("works when the lock file cannot be created", func(t *testing.T) {
		env := newTestEnv(t)
		notADir := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(notADir, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		env.manager.LockPath = filepath.Join(notADir, "token.lock")
		env.store.tokens = storedTokens(-time.Hour)
		env.github.refresh = tokensOK

		token, err := env.manager.Token(context.Background())
		if err != nil || token != "ghu_new" {
			t.Fatalf("Token() = %q, %v; want ghu_new", token, err)
		}
		if err := env.manager.Logout(); err != nil {
			t.Fatalf("Logout() = %v", err)
		}
	})

	t.Run("refresh failure keeps the stored tokens", func(t *testing.T) {
		env := newTestEnv(t)
		env.store.tokens = storedTokens(-time.Hour)
		env.github.refresh = map[string]any{"error": "server_error"}

		if _, err := env.manager.Token(context.Background()); err == nil {
			t.Fatal("Expected an error, got nil")
		}
		if env.store.saves != 0 || env.store.tokens.RefreshToken != "ghr_old" {
			t.Errorf("Stored tokens changed: %+v", env.store.tokens)
		}
	})

	t.Run("not logged in", func(t *testing.T) {
		env := newTestEnv(t)

		if _, err := env.manager.Token(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
			t.Fatalf("Expected ErrNotLoggedIn, got: %v", err)
		}
	})

	t.Run("concurrent callers refresh only once", func(t *testing.T) {
		env := newTestEnv(t)
		env.store.tokens = storedTokens(time.Minute)
		env.github.refresh = tokensOK
		env.github.refreshDelay = 50 * time.Millisecond

		const callers = 8
		tokens := make([]string, callers)
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tokens[i], errs[i] = env.manager.Token(context.Background())
			}()
		}
		wg.Wait()

		for i := range callers {
			if errs[i] != nil || tokens[i] != "ghu_new" {
				t.Errorf("Caller %d got %q, %v", i, tokens[i], errs[i])
			}
		}
		if n := len(env.github.refreshForms); n != 1 {
			t.Errorf("Refresh requests = %d, want 1", n)
		}
	})
}

func TestStatusAndLogout(t *testing.T) {
	env := newTestEnv(t)
	env.store.tokens = storedTokens(time.Hour)

	status, err := env.manager.Status()
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	want := Status{ClientID: "Iv23liTest", AccessTokenExpiresAt: testNow.Add(time.Hour), LoginExpiresAt: testNow.Add(90 * 24 * time.Hour)}
	if *status != want {
		t.Errorf("Status = %+v, want %+v", *status, want)
	}

	if err := env.manager.Logout(); err != nil {
		t.Fatalf("Logout() error: %v", err)
	}
	if env.store.tokens != nil {
		t.Error("Tokens still stored after logout")
	}
	if err := env.manager.Logout(); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("Second Logout() = %v, want ErrNotLoggedIn", err)
	}
	if _, err := env.manager.Status(); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("Status() after logout = %v, want ErrNotLoggedIn", err)
	}
}

func TestWait(t *testing.T) {
	if err := wait(context.Background(), time.Millisecond); err != nil {
		t.Errorf("wait() = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("wait() on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestNew(t *testing.T) {
	m, err := New()
	if runtime.GOOS != "darwin" {
		if err == nil {
			t.Fatal("Expected an error outside macOS")
		}
		return
	}
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if m.GitHub.BaseURL != "https://github.com" || filepath.Base(m.LockPath) != "token.lock" {
		t.Errorf("Unexpected manager: %+v", m)
	}
	if store, ok := m.Store.(*KeychainStore); !ok || store.Command != "/usr/bin/security" {
		t.Errorf("Store = %#v, want the Keychain store", m.Store)
	}
}
