package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Run("prints the token without a trailing newline", func(t *testing.T) {
		var gotAppID, gotInstallationID string
		getToken := func(appID, installationID string) (string, error) {
			gotAppID, gotInstallationID = appID, installationID
			return "ghs_test_token", nil
		}
		var stdout, stderr bytes.Buffer

		code := run([]string{"gh-app-token-generator", "12345", "67890"}, &stdout, &stderr, getToken)

		if code != 0 {
			t.Errorf("Exit code = %d, want 0", code)
		}
		if stdout.String() != "ghs_test_token" {
			t.Errorf("stdout = %q, want %q", stdout.String(), "ghs_test_token")
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty", stderr.String())
		}
		if gotAppID != "12345" || gotInstallationID != "67890" {
			t.Errorf("getToken called with (%q, %q), want (%q, %q)", gotAppID, gotInstallationID, "12345", "67890")
		}
	})

	t.Run("token error", func(t *testing.T) {
		getToken := func(string, string) (string, error) { return "", errors.New("keychain locked") }
		var stdout, stderr bytes.Buffer

		code := run([]string{"gh-app-token-generator", "12345", "67890"}, &stdout, &stderr, getToken)

		if code != 1 {
			t.Errorf("Exit code = %d, want 1", code)
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout = %q, want empty", stdout.String())
		}
		if !strings.Contains(stderr.String(), "Error:") || !strings.Contains(stderr.String(), "keychain locked") {
			t.Errorf("stderr = %q, want the error message", stderr.String())
		}
	})

	for _, args := range [][]string{
		{"gh-app-token-generator"},
		{"gh-app-token-generator", "12345"},
		{"gh-app-token-generator", "12345", "67890", "extra"},
	} {
		t.Run("usage with "+strings.Join(args[1:], ","), func(t *testing.T) {
			getToken := func(string, string) (string, error) {
				t.Error("getToken called despite wrong argument count")
				return "", nil
			}
			var stdout, stderr bytes.Buffer

			code := run(args, &stdout, &stderr, getToken)

			if code != 1 {
				t.Errorf("Exit code = %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "Usage:") {
				t.Errorf("stderr = %q, want usage", stderr.String())
			}
		})
	}
}
