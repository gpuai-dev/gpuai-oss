// Package cliauth stores and resolves the CLI's API key and API base URL.
//
// Credentials live at ~/.config/gpu/credentials.json with mode 0600. The
// $XDG_CONFIG_HOME env var overrides ~/.config when set.
package cliauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Credentials is the on-disk shape written by SaveCredentials.
type Credentials struct {
	APIKey   string    `json:"api_key"`
	APIBase  string    `json:"api_base,omitempty"`
	IssuedAt time.Time `json:"issued_at"`
}

// CredentialsPath returns the canonical path to credentials.json.
// Honors $XDG_CONFIG_HOME when set.
func CredentialsPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "gpu", "credentials.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "gpu", "credentials.json")
}

// SaveCredentials writes credentials.json atomically (tempfile + fsync + rename).
// File mode is set to 0600 BEFORE the rename. Parent dir is created 0700.
func SaveCredentials(c Credentials) error {
	path := CredentialsPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // best-effort cleanup if rename never happens
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadCredentials reads credentials.json. Returns an error wrapping
// fs.ErrNotExist when the file is absent.
func LoadCredentials() (Credentials, error) {
	path := CredentialsPath()
	b, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var c Credentials
	if jerr := json.Unmarshal(b, &c); jerr != nil {
		return Credentials{}, fmt.Errorf("parse %s: %w", path, jerr)
	}
	return c, nil
}

// RemoveCredentials deletes credentials.json. Idempotent — returns nil if
// the file does not exist.
func RemoveCredentials() error {
	err := os.Remove(CredentialsPath())
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ResolveToken returns the API key in precedence order:
//  1. GPUAI_API_KEY env var (source="env")
//  2. credentials.json or legacy config.toml (source="config")
//
// Returns ("", "", error) when neither source resolves.
func ResolveToken() (string, string, error) {
	if v := os.Getenv("GPUAI_API_KEY"); v != "" {
		return v, "env", nil
	}
	c, err := LoadCredentials()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", errors.New("no credentials — run `gpu login` or set GPUAI_API_KEY")
		}
		return "", "", err
	}
	if c.APIKey == "" {
		return "", "", errors.New("credentials file present but api_key empty — re-run `gpu login`")
	}
	return c.APIKey, "config", nil
}

// DefaultAPIBase is the API base used when nothing else is configured. It
// points at a gpuctl server running on the local machine.
const DefaultAPIBase = "http://localhost:8080/v1"

// ResolveAPIBase returns the API base URL in precedence order:
//  1. GPUAI_API_BASE env var (source="env")
//  2. api_base in credentials.json (source="config")
//  3. DefaultAPIBase (source="default")
//
// The --api-base flag, when given, overrides all three; that check lives in
// the caller because only it knows whether the flag was set.
func ResolveAPIBase() (string, string) {
	if v := os.Getenv("GPUAI_API_BASE"); v != "" {
		return v, "env"
	}
	if c, err := LoadCredentials(); err == nil && c.APIBase != "" {
		return c.APIBase, "config"
	}
	return DefaultAPIBase, "default"
}
