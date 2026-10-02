package authtoken

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	keychainService = "agent-github-token"
	keychainAccount = "github-app-user-token"

	// errSecItemNotFound is the exit status of `security` when the item does not exist.
	errSecItemNotFound = 44
)

// KeychainStore keeps Tokens as a generic password item in the macOS Keychain,
// using the `security` command-line tool.
type KeychainStore struct {
	Command string // path to the `security` tool
	Service string
	Account string
}

// NewKeychainStore returns a store for the default login Keychain.
func NewKeychainStore() *KeychainStore {
	return &KeychainStore{Command: "/usr/bin/security", Service: keychainService, Account: keychainAccount}
}

// Load reads the stored tokens. It returns ErrNotLoggedIn if there are none.
func (s *KeychainStore) Load() (*Tokens, error) {
	cmd := exec.Command(s.Command, "find-generic-password", "-s", s.Service, "-a", s.Account, "-w")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == errSecItemNotFound {
			return nil, ErrNotLoggedIn
		}
		return nil, fmt.Errorf("reading from the Keychain: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return decodeTokens(out)
}

// Save stores the tokens, replacing any stored before. The data goes to
// `security -i` on stdin, hex encoded, so the tokens never appear on a
// command line where other processes could see them.
func (s *KeychainStore) Save(t *Tokens) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	encoded := hex.EncodeToString(data)

	cmd := exec.Command(s.Command, "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -X %s\n", s.Service, s.Account, encoded))
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	// Never let the encoded tokens into an error message.
	details := strings.TrimSpace(strings.ReplaceAll(output.String(), encoded, "<redacted>"))
	if runErr != nil {
		return fmt.Errorf("writing to the Keychain: %w: %s", runErr, details)
	}

	// Read the item back to make sure the write took effect.
	saved, err := s.Load()
	if err != nil {
		return fmt.Errorf("writing to the Keychain: %w: %s", err, details)
	}
	if saved.AccessToken != t.AccessToken || saved.RefreshToken != t.RefreshToken {
		return fmt.Errorf("writing to the Keychain: stored item does not match: %s", details)
	}
	return nil
}

// Delete removes the stored tokens. It returns ErrNotLoggedIn if there are none.
func (s *KeychainStore) Delete() error {
	cmd := exec.Command(s.Command, "delete-generic-password", "-s", s.Service, "-a", s.Account)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == errSecItemNotFound {
			return ErrNotLoggedIn
		}
		return fmt.Errorf("deleting from the Keychain: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// decodeTokens parses a stored item. `security -w` prints the data as-is when
// it is printable and as hex otherwise, so both forms are accepted.
func decodeTokens(out []byte) (*Tokens, error) {
	data := bytes.TrimSpace(out)
	if len(data) > 0 && data[0] != '{' {
		decoded, err := hex.DecodeString(string(data))
		if err != nil {
			return nil, fmt.Errorf("the Keychain item is not valid; run login again")
		}
		data = decoded
	}
	var t Tokens
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("the Keychain item is not valid; run login again")
	}
	if t.ClientID == "" || t.AccessToken == "" || t.RefreshToken == "" {
		return nil, fmt.Errorf("the Keychain item is incomplete; run login again")
	}
	return &t, nil
}
