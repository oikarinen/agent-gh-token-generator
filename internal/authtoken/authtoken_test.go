package authtoken

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

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
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			token := "ghs_test_token"
			if err := json.NewEncoder(w).Encode(InstallationToken{Token: &token}); err != nil {
				t.Fatalf("Failed to encode token: %v", err)
			}
		}))
		defer server.Close()

		token, err := getInstallationAccessToken(server.URL, "12345", "dummy_jwt")
		if err != nil {
			t.Fatalf("Expected no error, but got: %v", err)
		}
		if *token != "ghs_test_token" {
			t.Errorf("Expected token 'ghs_test_token', but got '%s'", *token)
		}
	})

	t.Run("API error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		_, err := getInstallationAccessToken(server.URL, "12345", "dummy_jwt")
		if err == nil {
			t.Fatal("Expected an error, but got nil")
		}
	})

	t.Run("missing token in response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			if err := json.NewEncoder(w).Encode(InstallationToken{Token: nil}); err != nil {
				t.Fatalf("Failed to encode token: %v", err)
			}
		}))
		defer server.Close()

		_, err := getInstallationAccessToken(server.URL, "12345", "dummy_jwt")
		if err == nil {
			t.Fatal("Expected an error, but got nil")
		}
	})
}
