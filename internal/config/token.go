package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// ResolveToken returns the API token. The process environment wins, then the
// env file. A token is never read from config.yaml: config files get copied,
// committed, and pasted into issues.
//
// The env file's permissions are checked even when the token comes from the
// environment. systemd's EnvironmentFile= exports the file before this runs, so
// the check cannot live only on the read path.
func ResolveToken(envPath string) (string, error) {
	if envPath != "" {
		if info, err := os.Stat(envPath); err == nil {
			if perm := info.Mode().Perm(); perm&0o077 != 0 {
				return "", fmt.Errorf("token file %s must be mode 0600 (is %04o)", envPath, perm)
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("RUNLET_TOKEN")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); v != "" {
		return v, nil
	}
	if envPath == "" {
		return "", errors.New("no token: set RUNLET_TOKEN, or create ~/.config/runlet/env with GITHUB_TOKEN=...")
	}
	return LoadTokenFile(envPath)
}

// LoadTokenFile reads KEY=VALUE lines and refuses a file that other users can
// read. A token that is group- or world-readable is a misconfiguration, not a
// warning.
func LoadTokenFile(path string) (string, error) {
	return loadKey(path, "RUNLET_TOKEN", "GITHUB_TOKEN")
}

// ResolveAdminToken returns the credential used to mint registration tokens.
// It is a separate variable so the long-lived admin token is not the one used
// on every poll.
func ResolveAdminToken(envPath string) (string, error) {
	if envPath != "" {
		if info, err := os.Stat(envPath); err == nil {
			if perm := info.Mode().Perm(); perm&0o077 != 0 {
				return "", fmt.Errorf("token file %s must be mode 0600 (is %04o)", envPath, perm)
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("RUNLET_ADMIN_TOKEN")); v != "" {
		return v, nil
	}
	if envPath == "" {
		return "", errors.New("no admin token: set RUNLET_ADMIN_TOKEN")
	}
	return loadKey(envPath, "RUNLET_ADMIN_TOKEN")
}

func loadKey(path string, keys ...string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("token file %s must be mode 0600 (is %04o)", path, perm)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if !slices.Contains(keys, strings.TrimSpace(k)) {
			continue
		}
		if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
			return v, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return "", fmt.Errorf("no %s in %s", keys[0], path)
}
