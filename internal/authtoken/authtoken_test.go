package authtoken

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
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

// tokenHandler responds like GitHub does when an installation token is created.
func tokenHandler(t *testing.T, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(InstallationToken{Token: &token}); err != nil {
			t.Errorf("Failed to encode token: %v", err)
		}
	}
}

// generateKeyPEM returns a new RSA key and its PKCS#1 PEM encoding, the format
// GitHub uses for downloaded App private keys.
func generateKeyPEM(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	return privateKey, keyPEM
}

func TestCreateJWT(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}
	appID := "12345"

	tokenString, err := createJWT(appID, privateKey)
	if err != nil {
		t.Fatalf("createJWT failed: %v", err)
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return &privateKey.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("Failed to parse token: %v", err)
	}

	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		if issuer, ok := claims["iss"].(string); !ok || issuer != appID {
			t.Errorf("Expected issuer %s, got %v", appID, claims["iss"])
		}
		if exp, ok := claims["exp"].(float64); !ok || int64(exp) < time.Now().Unix() {
			t.Errorf("Token is expired or expiration is invalid")
		}
	} else {
		t.Errorf("Token is not valid or claims are not of type MapClaims")
	}
}

func TestGetInstallationAccessToken(t *testing.T) {
	t.Run("successful token retrieval", func(t *testing.T) {
		var got *http.Request
		client := handlerClient(func(w http.ResponseWriter, r *http.Request) {
			got = r
			tokenHandler(t, "ghs_test_token")(w, r)
		})

		token, err := getInstallationAccessToken(client, "https://api.example", "12345", "dummy_jwt")
		if err != nil {
			t.Fatalf("Expected no error, but got: %v", err)
		}
		if *token != "ghs_test_token" {
			t.Errorf("Expected token 'ghs_test_token', but got '%s'", *token)
		}

		if got.Method != http.MethodPost {
			t.Errorf("Expected POST, got %s", got.Method)
		}
		if got.URL.String() != "https://api.example/app/installations/12345/access_tokens" {
			t.Errorf("Unexpected URL: %s", got.URL)
		}
		wantHeaders := map[string]string{
			"Authorization":        "Bearer dummy_jwt",
			"Accept":               "application/vnd.github.v3+json",
			"X-GitHub-Api-Version": "2022-11-28",
		}
		for name, want := range wantHeaders {
			if v := got.Header.Get(name); v != want {
				t.Errorf("Header %s = %q, want %q", name, v, want)
			}
		}
		if got.Header.Get("User-Agent") == "" {
			t.Error("User-Agent header is not set")
		}
	})

	t.Run("API error", func(t *testing.T) {
		client := handlerClient(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("upstream exploded"))
		})

		_, err := getInstallationAccessToken(client, "https://api.example", "12345", "dummy_jwt")
		if err == nil {
			t.Fatal("Expected an error, but got nil")
		}
		if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "upstream exploded") {
			t.Errorf("Expected status code and body in error, got: %v", err)
		}
	})

	t.Run("missing token in response", func(t *testing.T) {
		client := handlerClient(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			if err := json.NewEncoder(w).Encode(InstallationToken{Token: nil}); err != nil {
				t.Errorf("Failed to encode token: %v", err)
			}
		})

		_, err := getInstallationAccessToken(client, "https://api.example", "12345", "dummy_jwt")
		if err == nil {
			t.Fatal("Expected an error, but got nil")
		}
	})

	t.Run("malformed JSON response", func(t *testing.T) {
		client := handlerClient(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{not json"))
		})

		_, err := getInstallationAccessToken(client, "https://api.example", "12345", "dummy_jwt")
		if err == nil {
			t.Fatal("Expected an error, but got nil")
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})}

		_, err := getInstallationAccessToken(client, "https://api.example", "12345", "dummy_jwt")
		if err == nil {
			t.Fatal("Expected an error, but got nil")
		}
	})

	t.Run("non-numeric installation ID is rejected before any request", func(t *testing.T) {
		called := false
		client := handlerClient(func(w http.ResponseWriter, r *http.Request) { called = true })

		_, err := getInstallationAccessToken(client, "https://api.example", "../../evil", "dummy_jwt")
		if err == nil {
			t.Fatal("Expected an error, but got nil")
		}
		if called {
			t.Error("A request was sent for an invalid installation ID")
		}
	})
}

func TestGetToken(t *testing.T) {
	privateKey, keyPEM := generateKeyPEM(t)
	readKey := func() ([]byte, error) { return keyPEM, nil }

	t.Run("exchanges a signed JWT for an installation token", func(t *testing.T) {
		client := handlerClient(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/app/installations/67890/access_tokens" {
				t.Errorf("Unexpected path: %s", r.URL.Path)
			}
			bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			token, err := jwt.ParseWithClaims(bearer, &jwt.RegisteredClaims{}, func(token *jwt.Token) (interface{}, error) {
				if token.Method != jwt.SigningMethodRS256 {
					return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
				}
				return &privateKey.PublicKey, nil
			})
			if err != nil {
				t.Errorf("JWT does not verify with the App's public key: %v", err)
			} else if iss := token.Claims.(*jwt.RegisteredClaims).Issuer; iss != "12345" {
				t.Errorf("JWT issuer = %q, want %q", iss, "12345")
			}
			tokenHandler(t, "ghs_end_to_end")(w, r)
		})

		token, err := getToken("12345", "67890", readKey, client, "https://api.example")
		if err != nil {
			t.Fatalf("Expected no error, but got: %v", err)
		}
		if token != "ghs_end_to_end" {
			t.Errorf("Expected token 'ghs_end_to_end', but got '%s'", token)
		}
	})

	t.Run("non-numeric app ID is rejected before reading the key", func(t *testing.T) {
		readCalled := false
		readKey := func() ([]byte, error) { readCalled = true; return keyPEM, nil }

		_, err := getToken("<Your-App-ID>", "67890", readKey, handlerClient(tokenHandler(t, "x")), "https://api.example")
		if err == nil || !strings.Contains(err.Error(), "invalid app ID") {
			t.Fatalf("Expected invalid app ID error, got: %v", err)
		}
		if readCalled {
			t.Error("The private key was read for an invalid app ID")
		}
	})

	t.Run("key read failure", func(t *testing.T) {
		errKeychain := errors.New("item not found")
		readKey := func() ([]byte, error) { return nil, errKeychain }

		_, err := getToken("12345", "67890", readKey, handlerClient(tokenHandler(t, "x")), "https://api.example")
		if !errors.Is(err, errKeychain) {
			t.Fatalf("Expected keychain error to be wrapped, got: %v", err)
		}
	})

	t.Run("invalid private key", func(t *testing.T) {
		readKey := func() ([]byte, error) { return []byte("not a PEM key"), nil }

		_, err := getToken("12345", "67890", readKey, handlerClient(tokenHandler(t, "x")), "https://api.example")
		if err == nil || !strings.Contains(err.Error(), "error parsing RSA private key") {
			t.Fatalf("Expected key parsing error, got: %v", err)
		}
	})

	t.Run("API failure", func(t *testing.T) {
		client := handlerClient(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})

		_, err := getToken("12345", "67890", readKey, client, "https://api.example")
		if err == nil || !strings.Contains(err.Error(), "error getting installation access token") {
			t.Fatalf("Expected API error, got: %v", err)
		}
	})
}

func TestGetTokenRejectsInvalidAppID(t *testing.T) {
	// The app ID is validated before the Keychain is read, so this is safe to
	// run on a developer machine.
	_, err := GetToken("<Your-App-ID>", "67890")
	if err == nil || !strings.Contains(err.Error(), "invalid app ID") {
		t.Fatalf("Expected invalid app ID error, got: %v", err)
	}
}

func TestGetPrivateKeyFromKeychainOutsideMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("would read the real macOS Keychain")
	}
	_, err := getPrivateKeyFromKeychain()
	if err == nil || !strings.Contains(err.Error(), "only supported on macOS") {
		t.Fatalf("Expected macOS-only error, got: %v", err)
	}
}

func TestDecodeKeychainKey(t *testing.T) {
	keyPEM := []byte("-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAK\n-----END RSA PRIVATE KEY-----\n")
	encoded := base64.StdEncoding.EncodeToString(keyPEM)

	tests := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{name: "as printed by security, with trailing newline", output: encoded + "\n"},
		{name: "wrapped at 64 columns", output: encoded[:64] + "\n" + encoded[64:] + "\n"},
		{name: "CRLF line endings", output: encoded[:64] + "\r\n" + encoded[64:] + "\r\n"},
		{name: "empty output", output: "", wantErr: true},
		{name: "not base64", output: "this is not base64!\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeKeychainKey([]byte(tt.output))
			if tt.wantErr {
				if err == nil {
					t.Fatal("Expected an error, but got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}
			if string(got) != string(keyPEM) {
				t.Errorf("Decoded key = %q, want %q", got, keyPEM)
			}
		})
	}
}
