package authtoken

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSecurity is a stand-in for the macOS `security` tool. It keeps one item
// in a file, logs every command line, and fails on request.
const fakeSecurity = `#!/bin/bash
dir="$(dirname "$0")"
echo "$*" >> "$dir/argv.log"
if [[ "$1" == "-i" ]]; then
  read -r -a cmd
  set -- "${cmd[@]}"
  if [[ -f "$dir/fail-add" ]]; then
    echo "security: could not add: ${cmd[*]}" >&2
    exit 0  # like security -i, report the error but exit 0
  fi
fi
if [[ -f "$dir/broken" ]]; then
  echo "security: keychain is locked" >&2
  exit 51
fi
case "$1" in
  add-generic-password)
    while [[ $# -gt 0 ]]; do
      [[ "$1" == "-X" ]] && printf '%s' "$2" > "$dir/item"
      shift
    done ;;
  find-generic-password)
    if [[ ! -f "$dir/item" ]]; then
      echo "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain." >&2
      exit 44
    fi
    cat "$dir/item"; echo ;;
  delete-generic-password)
    [[ -f "$dir/item" ]] || exit 44
    rm "$dir/item" ;;
  *) echo "unexpected command: $*" >&2; exit 2 ;;
esac
`

func newFakeKeychain(t *testing.T) (*KeychainStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "security")
	if err := os.WriteFile(path, []byte(fakeSecurity), 0o755); err != nil {
		t.Fatal(err)
	}
	return &KeychainStore{Command: path, Service: keychainService, Account: keychainAccount}, dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKeychainStore(t *testing.T) {
	tokens := &Tokens{
		ClientID:              "Iv23liTest",
		AccessToken:           "ghu_secret_access",
		AccessTokenExpiresAt:  testNow.Add(8 * time.Hour),
		RefreshToken:          "ghr_secret_refresh",
		RefreshTokenExpiresAt: testNow.Add(180 * 24 * time.Hour),
	}

	t.Run("save, load and delete", func(t *testing.T) {
		store, dir := newFakeKeychain(t)

		if _, err := store.Load(); !errors.Is(err, ErrNotLoggedIn) {
			t.Fatalf("Load() before save = %v, want ErrNotLoggedIn", err)
		}
		if err := store.Save(tokens); err != nil {
			t.Fatalf("Save() error: %v", err)
		}
		loaded, err := store.Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if !loaded.AccessTokenExpiresAt.Equal(tokens.AccessTokenExpiresAt) || loaded.AccessToken != tokens.AccessToken ||
			loaded.RefreshToken != tokens.RefreshToken || loaded.ClientID != tokens.ClientID {
			t.Errorf("Loaded %+v, want %+v", loaded, tokens)
		}
		if err := store.Delete(); err != nil {
			t.Fatalf("Delete() error: %v", err)
		}
		if err := store.Delete(); !errors.Is(err, ErrNotLoggedIn) {
			t.Errorf("Second Delete() = %v, want ErrNotLoggedIn", err)
		}

		argv := readFile(t, filepath.Join(dir, "argv.log"))
		for _, want := range []string{
			"find-generic-password -s agent-github-token -a github-app-user-token -w",
			"delete-generic-password -s agent-github-token -a github-app-user-token",
			"-i",
		} {
			if !strings.Contains(argv, want) {
				t.Errorf("Command lines do not include %q:\n%s", want, argv)
			}
		}
		if strings.Contains(argv, "ghu_secret") || strings.Contains(argv, hex.EncodeToString([]byte("ghu_secret"))) {
			t.Errorf("Tokens appeared on a command line:\n%s", argv)
		}
	})

	t.Run("failed write is detected and does not leak the tokens", func(t *testing.T) {
		store, dir := newFakeKeychain(t)
		touch(t, filepath.Join(dir, "fail-add"))

		err := store.Save(tokens)
		if err == nil {
			t.Fatal("Expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "could not add") {
			t.Errorf("Error does not include the tool's output: %v", err)
		}
		if strings.Contains(err.Error(), hex.EncodeToString([]byte(`"ghu_secret`))[:20]) || strings.Contains(err.Error(), "ghu_secret") {
			t.Errorf("Error leaks the tokens: %v", err)
		}
	})

	t.Run("other errors include the tool's message", func(t *testing.T) {
		store, dir := newFakeKeychain(t)
		touch(t, filepath.Join(dir, "broken"))

		for name, err := range map[string]error{"Load": func() error { _, err := store.Load(); return err }(), "Delete": store.Delete()} {
			if err == nil || errors.Is(err, ErrNotLoggedIn) || !strings.Contains(err.Error(), "keychain is locked") {
				t.Errorf("%s() = %v, want the tool's error", name, err)
			}
		}
	})
}

func TestDecodeTokens(t *testing.T) {
	tokens := Tokens{ClientID: "Iv23liTest", AccessToken: "ghu_a", RefreshToken: "ghr_r", AccessTokenExpiresAt: testNow}
	data, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name    string
		output  string
		wantErr bool
	}{
		{name: "plain JSON, as security prints printable data", output: string(data) + "\n"},
		{name: "hex, as security prints other data", output: hex.EncodeToString(data) + "\n"},
		{name: "empty", output: "", wantErr: true},
		{name: "not hex", output: "zz-not-hex\n", wantErr: true},
		{name: "not JSON", output: "{broken\n", wantErr: true},
		{name: "incomplete", output: `{"client_id":"Iv23liTest","access_token":"ghu_a"}`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeTokens([]byte(tt.output))
			if tt.wantErr {
				if err == nil {
					t.Fatal("Expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Expected no error, got: %v", err)
			}
			if got.AccessToken != "ghu_a" || got.RefreshToken != "ghr_r" || !got.AccessTokenExpiresAt.Equal(testNow) {
				t.Errorf("Decoded %+v", got)
			}
		})
	}
}
