package authtoken

import (
	"bytes"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

// The structure of the JSON response from GitHub API
type InstallationToken struct {
	Token *string `json:"token"`
}

// GetToken generates and returns a GitHub App installation access token.
func GetToken(appID, installationID string) (string, error) {
	if _, err := strconv.ParseInt(appID, 10, 64); err != nil {
		return "", fmt.Errorf("invalid app ID: %s", appID)
	}

	// Fetch the private key from macOS Keychain
	keyBytes, err := getPrivateKeyFromKeychain()
	if err != nil {
		return "", fmt.Errorf("error fetching private key from keychain: %w", err)
	}

	// Parse the RSA private key from the PEM format
	privateKey, err := jwt.ParseRSAPrivateKeyFromPEM(keyBytes)
	if err != nil {
		return "", fmt.Errorf("error parsing RSA private key: %w", err)
	}

	// Generate the JWT
	signedToken, err := createJWT(appID, privateKey)
	if err != nil {
		return "", fmt.Errorf("error creating JWT: %w", err)
	}

	// Get the installation access token from GitHub API
	accessToken, err := getInstallationAccessToken("https://api.github.com", installationID, signedToken)
	if err != nil {
		return "", fmt.Errorf("error getting installation access token: %w", err)
	}

	return *accessToken, nil
}

// getPrivateKeyFromKeychain retrieves the GitHub App private key from the macOS Keychain.
func getPrivateKeyFromKeychain() ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("private key retrieval from keychain is only supported on macOS")
	}
	cmd := exec.Command("security", "find-generic-password", "-a", "GH_APP_PRIVATE_KEY", "-s", "agent-github-token", "-w")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("keychain command failed: %w", err)
	}
	if len(output) == 0 {
		return nil, fmt.Errorf("private key not found in keychain")
	}

	// Remove any embedded newlines, as base64.StdEncoding.DecodeString does not tolerate them.
	cleanedOutput := bytes.ReplaceAll(output, []byte("\n"), nil)
	cleanedOutput = bytes.ReplaceAll(cleanedOutput, []byte("\r"), nil)

	decoded, err := base64.StdEncoding.DecodeString(string(cleanedOutput))
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64 private key from keychain: %w", err)
	}
	return decoded, nil
}

// createJWT creates and signs a new JWT for authenticating as a GitHub App.
func createJWT(appID string, privateKey *rsa.PrivateKey) (string, error) {
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(10 * time.Minute)),
		Issuer:    appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(privateKey)
}

// getInstallationAccessToken uses the JWT to request an installation access token from GitHub.
func getInstallationAccessToken(baseURL, installationID, jwtToken string) (*string, error) {
	// Validate that installationID is a number to prevent theoretical SSRF
	if _, err := strconv.ParseInt(installationID, 10, 64); err != nil {
		return nil, fmt.Errorf("invalid installation ID: %s", installationID)
	}

	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", baseURL, installationID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "github-app-authtoken-client/1.0")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("received non-201 status code: %d (%s) - Body: %s", resp.StatusCode, resp.Status, string(body))
	}

	var it InstallationToken
	if err := json.NewDecoder(resp.Body).Decode(&it); err != nil {
		return nil, err
	}

	if it.Token == nil {
		return nil, fmt.Errorf("token field is missing from API response")
	}

	return it.Token, nil
}
