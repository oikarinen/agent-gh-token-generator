package authtoken

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
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
	accessToken, err := getInstallationAccessToken(installationID, signedToken)
	if err != nil {
		return "", fmt.Errorf("error getting installation access token: %w", err)
	}

	return *accessToken, nil
}

// getPrivateKeyFromKeychain retrieves the GitHub App private key from the macOS Keychain.
func getPrivateKeyFromKeychain() ([]byte, error) {
	cmd := exec.Command("security", "find-generic-password", "-a", "GH_APP_PRIVATE_KEY", "-s", "agent-github-token", "-w")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("keychain command failed: %v - %s", err, string(output))
	}
	if len(output) == 0 {
		return nil, fmt.Errorf("private key not found in keychain")
	}
	return output, nil
}

// createJWT creates and signs a new JWT for authenticating as a GitHub App.
func createJWT(appID string, privateKey interface{}) (string, error) {
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(10 * time.Minute)),
		Issuer:    appID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(privateKey)
}

// getInstallationAccessToken uses the JWT to request an installation access token from GitHub.
func getInstallationAccessToken(installationID, jwtToken string) (*string, error) {
	// Validate that installationID is a number to prevent theoretical SSRF
	if _, err := strconv.ParseInt(installationID, 10, 64); err != nil {
		return nil, fmt.Errorf("invalid installation ID: %s", installationID)
	}

	url := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", installationID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("received non-201 status code: %d", resp.StatusCode)
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
