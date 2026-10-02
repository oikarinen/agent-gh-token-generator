package authtoken

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// handlerClient returns an HTTP client that serves every request with h
// directly, without opening a network listener.
func handlerClient(h http.HandlerFunc) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec.Result(), nil
	})}
}

// jsonHandler responds with status and body encoded as JSON, recording the
// request in *got.
func jsonHandler(t *testing.T, got **http.Request, form *url.Values, status int, body any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("Failed to parse form: %v", err)
		}
		if got != nil {
			*got = r
		}
		if form != nil {
			*form = r.PostForm
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("Failed to encode response: %v", err)
		}
	}
}

func testGitHub(h http.HandlerFunc) *GitHub {
	return &GitHub{BaseURL: "https://github.example", Client: handlerClient(h)}
}

func TestRequestDeviceCode(t *testing.T) {
	t.Run("sends the client ID and decodes the response", func(t *testing.T) {
		var req *http.Request
		var form url.Values
		gh := testGitHub(jsonHandler(t, &req, &form, http.StatusOK, map[string]any{
			"device_code":      "dev123",
			"user_code":        "ABCD-1234",
			"verification_uri": "https://github.com/login/device",
			"expires_in":       900,
			"interval":         5,
		}))

		dc, err := gh.RequestDeviceCode(context.Background(), "Iv23liTest")
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		want := DeviceCode{DeviceCode: "dev123", UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 5}
		if *dc != want {
			t.Errorf("DeviceCode = %+v, want %+v", *dc, want)
		}
		if req.Method != http.MethodPost || req.URL.String() != "https://github.example/login/device/code" {
			t.Errorf("Request = %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if got := form.Get("client_id"); got != "Iv23liTest" {
			t.Errorf("client_id = %q", got)
		}
		if _, ok := form["client_secret"]; ok {
			t.Error("client_secret must not be sent")
		}
	})

	t.Run("OAuth error", func(t *testing.T) {
		gh := testGitHub(jsonHandler(t, nil, nil, http.StatusOK, map[string]any{
			"error":             "device_flow_disabled",
			"error_description": "Device Flow must be explicitly enabled for this App",
		}))

		_, err := gh.RequestDeviceCode(context.Background(), "Iv23liTest")
		var oauthErr *OAuthError
		if !errors.As(err, &oauthErr) || oauthErr.Code != "device_flow_disabled" {
			t.Fatalf("Expected device_flow_disabled OAuthError, got: %v", err)
		}
	})

	t.Run("incomplete response", func(t *testing.T) {
		gh := testGitHub(jsonHandler(t, nil, nil, http.StatusOK, map[string]any{"device_code": "dev123"}))

		if _, err := gh.RequestDeviceCode(context.Background(), "Iv23liTest"); err == nil {
			t.Fatal("Expected an error, got nil")
		}
	})
}

func TestUserInstallations(t *testing.T) {
	t.Run("asks with the token and decodes installations", func(t *testing.T) {
		var req *http.Request
		gh := &GitHub{APIBaseURL: "https://api.github.example", Client: handlerClient(jsonHandler(t, &req, nil, http.StatusOK, map[string]any{
			"total_count":   1,
			"installations": []map[string]any{{"id": 7, "client_id": "Iv23liTest", "app_slug": "my-agent", "app_id": 42}},
		}))}

		installations, err := gh.UserInstallations(context.Background(), "ghu_abc")
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		if len(installations) != 1 || installations[0] != (Installation{ID: 7, ClientID: "Iv23liTest", AppSlug: "my-agent"}) {
			t.Errorf("Installations = %+v", installations)
		}
		if req.Method != http.MethodGet || req.URL.Path != "/user/installations" {
			t.Errorf("Request = %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer ghu_abc" {
			t.Errorf("Authorization = %q", got)
		}
		if got := req.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
	})

	t.Run("rejected token", func(t *testing.T) {
		gh := &GitHub{APIBaseURL: "https://api.github.example", Client: handlerClient(jsonHandler(t, nil, nil, http.StatusForbidden,
			map[string]any{"message": "You must authenticate with an access token authorized to a GitHub App"}))}

		if _, err := gh.UserInstallations(context.Background(), "ghp_classic"); err == nil {
			t.Fatal("Expected an error, got nil")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		gh := &GitHub{APIBaseURL: "https://api.github.example", Client: handlerClient(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{not json"))
		})}

		if _, err := gh.UserInstallations(context.Background(), "ghu_abc"); err == nil {
			t.Fatal("Expected an error, got nil")
		}
	})
}

func TestTokenRequests(t *testing.T) {
	success := map[string]any{
		"access_token":             "ghu_new",
		"expires_in":               28800,
		"refresh_token":            "ghr_new",
		"refresh_token_expires_in": 15897600,
		"token_type":               "bearer",
		"scope":                    "",
	}

	t.Run("device token poll", func(t *testing.T) {
		var req *http.Request
		var form url.Values
		gh := testGitHub(jsonHandler(t, &req, &form, http.StatusOK, success))

		resp, err := gh.PollDeviceToken(context.Background(), "Iv23liTest", "dev123")
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		want := tokenResponse{AccessToken: "ghu_new", ExpiresIn: 28800, RefreshToken: "ghr_new", RefreshTokenExpiresIn: 15897600}
		if *resp != want {
			t.Errorf("Response = %+v, want %+v", *resp, want)
		}
		if req.URL.Path != "/login/oauth/access_token" {
			t.Errorf("Path = %s", req.URL.Path)
		}
		wantForm := url.Values{
			"client_id":   {"Iv23liTest"},
			"device_code": {"dev123"},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}
		if form.Encode() != wantForm.Encode() {
			t.Errorf("Form = %s, want %s", form.Encode(), wantForm.Encode())
		}
	})

	t.Run("refresh without client secret", func(t *testing.T) {
		var form url.Values
		gh := testGitHub(jsonHandler(t, nil, &form, http.StatusOK, success))

		if _, err := gh.RefreshToken(context.Background(), "Iv23liTest", "ghr_old"); err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		wantForm := url.Values{
			"client_id":     {"Iv23liTest"},
			"grant_type":    {"refresh_token"},
			"refresh_token": {"ghr_old"},
		}
		if form.Encode() != wantForm.Encode() {
			t.Errorf("Form = %s, want %s", form.Encode(), wantForm.Encode())
		}
	})

	t.Run("slow_down carries the new interval", func(t *testing.T) {
		gh := testGitHub(jsonHandler(t, nil, nil, http.StatusOK, map[string]any{"error": "slow_down", "interval": 10}))

		_, err := gh.PollDeviceToken(context.Background(), "Iv23liTest", "dev123")
		var oauthErr *OAuthError
		if !errors.As(err, &oauthErr) || oauthErr.Code != "slow_down" || oauthErr.Interval != 10 {
			t.Fatalf("Expected slow_down with interval 10, got: %#v", err)
		}
	})

	t.Run("OAuth error with non-200 status", func(t *testing.T) {
		gh := testGitHub(jsonHandler(t, nil, nil, http.StatusBadRequest, map[string]any{"error": "bad_refresh_token"}))

		_, err := gh.RefreshToken(context.Background(), "Iv23liTest", "ghr_old")
		var oauthErr *OAuthError
		if !errors.As(err, &oauthErr) || oauthErr.Code != "bad_refresh_token" {
			t.Fatalf("Expected bad_refresh_token, got: %v", err)
		}
	})

	t.Run("non-200 without an OAuth error", func(t *testing.T) {
		gh := testGitHub(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		})

		if _, err := gh.RefreshToken(context.Background(), "Iv23liTest", "ghr_old"); err == nil {
			t.Fatal("Expected an error, got nil")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		gh := testGitHub(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{not json"))
		})

		if _, err := gh.RefreshToken(context.Background(), "Iv23liTest", "ghr_old"); err == nil {
			t.Fatal("Expected an error, got nil")
		}
	})

	t.Run("missing access token", func(t *testing.T) {
		gh := testGitHub(jsonHandler(t, nil, nil, http.StatusOK, map[string]any{"token_type": "bearer"}))

		if _, err := gh.RefreshToken(context.Background(), "Iv23liTest", "ghr_old"); err == nil {
			t.Fatal("Expected an error, got nil")
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		gh := &GitHub{BaseURL: "https://github.example", Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})}}

		if _, err := gh.RefreshToken(context.Background(), "Iv23liTest", "ghr_old"); err == nil {
			t.Fatal("Expected an error, got nil")
		}
	})
}
