package syncclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Token is the persisted OAuth token set.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

// expirySkew is the safety margin applied by Valid: a token expiring within
// this window is treated as already expired. Without it, a token valid at the
// check can expire while the request is in flight, surfacing as a bare
// "status 401" instead of a clean refresh.
const expirySkew = 60 * time.Second

// Valid reports whether the access token is present and will remain unexpired
// for at least expirySkew.
func (t Token) Valid() bool {
	return t.AccessToken != "" && time.Now().Add(expirySkew).Before(t.Expiry)
}

// DefaultTokenPath is ~/.config/orbeat/credentials.json.
func DefaultTokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("token path: %w", err)
	}
	return filepath.Join(home, ".config", "orbeat", "credentials.json"), nil
}

// SaveToken writes the token to path with 0600 perms (0700 parent dir).
func SaveToken(path string, t Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save token: mkdir: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save token: chmod dir: %w", err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("save token: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("save token: write: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("save token: chmod: %w", err)
	}
	return nil
}

// LoadToken reads the token from path.
func LoadToken(path string) (Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Token{}, fmt.Errorf("load token: %w", err)
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return Token{}, fmt.Errorf("load token: parse: %w", err)
	}
	return t, nil
}

// ClearToken removes the token file (no error if already absent).
func ClearToken(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear token: %w", err)
	}
	return nil
}
