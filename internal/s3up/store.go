package s3up

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SecretStore keeps the secret key out of the config file.
type SecretStore interface {
	Get(account string) (string, error)
	Set(account, secret string) error
}

// Runner executes a command and returns its output; a seam for tests.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Keychain stores secrets in the macOS login keychain with `security`.
type Keychain struct {
	Service string
	Run     Runner
}

// NewKeychain returns a Keychain store for service.
func NewKeychain(service string) *Keychain {
	return &Keychain{Service: service, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}}
}

// Get returns the secret saved for account.
func (k *Keychain) Get(account string) (string, error) {
	out, err := k.Run(context.Background(), "security", "find-generic-password", "-s", k.Service, "-a", account, "-w")
	if err != nil {
		return "", fmt.Errorf("keychain: không có secret cho %q", account)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// Set saves the secret for account, replacing an older one.
func (k *Keychain) Set(account, secret string) error {
	if _, err := k.Run(context.Background(), "security", "add-generic-password", "-U", "-s", k.Service, "-a", account, "-w", secret); err != nil {
		return fmt.Errorf("keychain: %w", err)
	}
	return nil
}

// ConfigPath is where the non-secret settings live.
func ConfigPath(app string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, app, "s3.json"), nil
}

// LoadConfig reads the settings and the secret. A missing file returns an
// empty config and no error.
func LoadConfig(path string, secrets SecretStore) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.AccessKey != "" && secrets != nil {
		cfg.SecretKey, _ = secrets.Get(cfg.AccessKey)
	}
	return cfg, nil
}

// SaveConfig writes the settings, and the secret to the store.
func SaveConfig(path string, cfg Config, secrets SecretStore) error {
	if cfg.SecretKey != "" && secrets != nil {
		if err := secrets.Set(cfg.AccessKey, cfg.SecretKey); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
