package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
server:
  listen: "127.0.0.1:9464"
policy:
  mode: dry-run
  poll: 30s
  cancel_cap_per_hour: 20
  grace_seconds: 45
pool:
  state_dir: /tmp/runlet-state
  work_dir: /tmp/runlet-work
  runner_dir: RUNNER_DIR_PLACEHOLDER
  runner_url: "https://github.com/your-org"
  memory_budget: 3.5GiB
  lanes:
    - name: lane-0
      labels: [self-hosted, linux, x64]
      memory: 1.5GiB
      memory_high: 1.2GiB
      cpu_quota: "250%"
repos:
  - slug: octarq-org/octarq-pro
    cancel_superseded: true
`

// fakeRunnerDir is the shape checkRunnerDir expects from an extracted runner.
func fakeRunnerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"config.sh", "run.sh", filepath.Join("bin", "Runner.Listener")} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	body = strings.ReplaceAll(body, "RUNNER_DIR_PLACEHOLDER", fakeRunnerDir(t))
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Policy.ModeValue() != "dry-run" {
		t.Fatalf("mode = %q", cfg.Policy.ModeValue())
	}
	if got, want := cfg.Policy.PollEvery(), 30*time.Second; got != want {
		t.Fatalf("poll = %s, want %s", got, want)
	}
	if got, want := cfg.Pool.Lanes[0].Memory.Int64(), int64(1.5*1024*1024*1024); got != want {
		t.Fatalf("lane memory = %d, want %d", got, want)
	}
}

func TestDefaultsAreSafe(t *testing.T) {
	body := strings.Replace(validYAML, "  mode: dry-run\n", "", 1)
	body = strings.Replace(body, "  poll: 30s\n", "", 1)
	body = strings.Replace(body, "  grace_seconds: 45\n", "", 1)
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Policy.ModeValue() != "dry-run" {
		t.Fatalf("mode default = %q, want dry-run", cfg.Policy.ModeValue())
	}
	if cfg.Policy.PollEvery() != 30*time.Second {
		t.Fatalf("poll default = %s", cfg.Policy.PollEvery())
	}
	if cfg.Policy.Grace() != 45*time.Second {
		t.Fatalf("grace default = %s", cfg.Policy.Grace())
	}
}

// TestExplicitZeroIsRejected is the fail-closed half of the defaults: an absent
// key takes a default, an explicit zero is a mistake and must not be rewritten.
func TestExplicitZeroIsRejected(t *testing.T) {
	cases := map[string]string{
		"cancel cap": strings.Replace(validYAML, "cancel_cap_per_hour: 20", "cancel_cap_per_hour: 0", 1),
		"poll":       strings.Replace(validYAML, "poll: 30s", "poll: 0s", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeTemp(t, body)); err == nil {
				t.Fatal("want an error for an explicit zero")
			}
		})
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	_, err := Load(writeTemp(t, validYAML+"\nbogus: true\n"))
	if err == nil || !strings.Contains(err.Error(), "field bogus not found") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty repos", strings.Replace(validYAML, "repos:\n  - slug: octarq-org/octarq-pro\n    cancel_superseded: true\n", "repos: []\n", 1), "repos must not be empty"},
		{"bad slug", strings.Replace(validYAML, "octarq-org/octarq-pro", "not-a-slug", 1), "must look like owner/name"},
		{"bad mode", strings.Replace(validYAML, "mode: dry-run", "mode: maybe", 1), "policy.mode"},
		{"lane over budget", strings.Replace(validYAML, "memory: 1.5GiB", "memory: 4GiB", 1), "exceeds pool.memory_budget"},
		{"high above max", strings.Replace(validYAML, "memory_high: 1.2GiB", "memory_high: 2GiB", 1), "must not exceed memory"},
		{"duplicate repo", validYAML + "  - slug: octarq-org/octarq-pro\n    cancel_superseded: true\n", "listed twice"},
		{"all interfaces", strings.Replace(validYAML, `listen: "127.0.0.1:9464"`, `listen: "0.0.0.0:9464"`, 1), "loopback"},
		{"not host:port", strings.Replace(validYAML, `listen: "127.0.0.1:9464"`, `listen: "127.0.0.1"`, 1), "host:port"},
		{"runner url not https", strings.Replace(validYAML, `runner_url: "https://github.com/your-org"`, `runner_url: "http://github.com/your-org"`, 1), "must use https"},
		{"runner dir missing", strings.Replace(validYAML, "RUNNER_DIR_PLACEHOLDER", "/nonexistent-runner", 1), "pool.runner_dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTemp(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestTokenFileMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte("GITHUB_TOKEN=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTokenFile(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("want 0600 error, got %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := LoadTokenFile(path)
	if err != nil || tok != "secret" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
}

func TestResolveTokenEnvWins(t *testing.T) {
	t.Setenv("RUNLET_TOKEN", "from-env")
	t.Setenv("GITHUB_TOKEN", "")
	tok, err := ResolveToken("/nonexistent")
	if err != nil || tok != "from-env" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
}

// TestResolveTokenChecksFilePermissions: systemd's EnvironmentFile= exports the
// file before runlet runs. The permission check must still fire, or a
// world-readable token is used without a word.
func TestResolveTokenChecksFilePermissions(t *testing.T) {
	t.Setenv("RUNLET_TOKEN", "from-env")
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("GITHUB_TOKEN=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveToken(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("want 0600 error, got %v", err)
	}
}

func TestParseScope(t *testing.T) {
	cases := map[string]struct {
		path string
		ok   bool
	}{
		"https://github.com/acme":       {"/orgs/acme/actions/runners", true},
		"https://github.com/acme/repo":  {"/repos/acme/repo/actions/runners", true},
		"acme":                          {"/orgs/acme/actions/runners", true},
		"acme/repo":                     {"/repos/acme/repo/actions/runners", true},
		"http://github.com/acme":        {"", false},
		"https://github.com/a/b/c":      {"", false},
		"https://gitlab.example/acme/x": {"/repos/acme/x/actions/runners", true},
	}
	for in, want := range cases {
		scope, err := ParseScope(in)
		if want.ok && err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if !want.ok {
			if err == nil {
				t.Fatalf("%q: want an error", in)
			}
			continue
		}
		if got := scope.APIPath(); got != want.path {
			t.Fatalf("%q = %q, want %q", in, got, want.path)
		}
	}
}

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{
		"1024":    1024,
		"1KiB":    1024,
		"1.5GiB":  1.5 * 1024 * 1024 * 1024,
		"2GB":     2_000_000_000,
		"512 MiB": 512 * 1024 * 1024,
		"1TiB":    1024 * 1024 * 1024 * 1024,
	}
	for in, want := range cases {
		got, err := ParseBytes(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if int64(got) != int64(want) {
			t.Fatalf("%q = %d, want %d", in, got, want)
		}
	}
	if _, err := ParseBytes("three gigs"); err == nil {
		t.Fatal("want error for junk size")
	}
}
