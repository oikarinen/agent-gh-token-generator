package authtoken

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const userAgent = "agent-gh-token-generator"

// GitHub calls GitHub's OAuth endpoints for the device flow and token
// refresh, and its REST API to check which App a token belongs to.
type GitHub struct {
	BaseURL    string // https://github.com
	APIBaseURL string // https://api.github.com
	Client     *http.Client
}

// Installation is an installation of a GitHub App.
type Installation struct {
	ID       int64  `json:"id"`
	ClientID string `json:"client_id"`
	AppSlug  string `json:"app_slug"`
}

// UserInstallations lists the installations, accessible to the user, of the
// GitHub App that issued token. GitHub only returns installations of that
// App, so their client IDs identify which App the token belongs to.
func (g *GitHub) UserInstallations(ctx context.Context, token string) ([]Installation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.APIBaseURL+"/user/installations?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected response from GitHub /user/installations: %s", resp.Status)
	}
	var body struct {
		Installations []Installation `json:"installations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding response from GitHub /user/installations: %w", err)
	}
	return body.Installations, nil
}

// DeviceCode is GitHub's response to starting the device flow.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// tokenResponse is GitHub's response from the access token endpoint.
type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
}

// OAuthError is an error code returned by GitHub's OAuth endpoints, such as
// "authorization_pending" or "bad_refresh_token".
type OAuthError struct {
	Code        string
	Description string
	Interval    int // for "slow_down": the new minimum polling interval in seconds
}

func (e *OAuthError) Error() string {
	if e.Description == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Description)
}

// RequestDeviceCode starts the device flow for the GitHub App with clientID.
func (g *GitHub) RequestDeviceCode(ctx context.Context, clientID string) (*DeviceCode, error) {
	var dc DeviceCode
	if err := g.post(ctx, "/login/device/code", url.Values{"client_id": {clientID}}, &dc); err != nil {
		return nil, err
	}
	if dc.DeviceCode == "" || dc.UserCode == "" || dc.VerificationURI == "" {
		return nil, fmt.Errorf("incomplete device code response from GitHub")
	}
	return &dc, nil
}

// PollDeviceToken asks once whether the user has entered the device code.
// Until they have, it returns an *OAuthError with code "authorization_pending".
func (g *GitHub) PollDeviceToken(ctx context.Context, clientID, deviceCode string) (*tokenResponse, error) {
	return g.requestToken(ctx, url.Values{
		"client_id":   {clientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	})
}

// RefreshToken exchanges a refresh token for a new access token and refresh
// token. GitHub invalidates the old pair once this succeeds. Tokens from the
// device flow can be refreshed without the App's client secret.
func (g *GitHub) RefreshToken(ctx context.Context, clientID, refreshToken string) (*tokenResponse, error) {
	return g.requestToken(ctx, url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (g *GitHub) requestToken(ctx context.Context, values url.Values) (*tokenResponse, error) {
	var tr tokenResponse
	if err := g.post(ctx, "/login/oauth/access_token", values, &tr); err != nil {
		return nil, err
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("access token missing from GitHub response")
	}
	return &tr, nil
}

// post sends a form-encoded POST and decodes the JSON response into out.
// GitHub reports OAuth errors in the body, often with status 200, so the body
// is checked for an error code before decoding.
func (g *GitHub) post(ctx context.Context, path string, values url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+path, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := g.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	var oauthErr struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Interval         int    `json:"interval"`
	}
	if json.Unmarshal(body, &oauthErr) == nil && oauthErr.Error != "" {
		return &OAuthError{Code: oauthErr.Error, Description: oauthErr.ErrorDescription, Interval: oauthErr.Interval}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected response from GitHub %s: %s", path, resp.Status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding response from GitHub %s: %w", path, err)
	}
	return nil
}
