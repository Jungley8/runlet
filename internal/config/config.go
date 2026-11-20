package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole file. Unknown keys are rejected by Load.
type Config struct {
	Server ServerConfig `yaml:"server"`
	Policy PolicyConfig `yaml:"policy"`
	Pool   PoolConfig   `yaml:"pool"`
	Repos  []RepoConfig `yaml:"repos"`
}

type ServerConfig struct {
	// Listen must be a loopback address. The /metrics and /status endpoints are
	// not authenticated, so exposing them is not a supported configuration.
	Listen string `yaml:"listen"`
}

// PolicyConfig uses pointers so that an absent key and an explicit zero are
// different things. Absent means "use the safe default"; an explicit zero is a
// mistake in the file and is rejected. A plain int cannot tell them apart, and
// rewriting `cancel_cap_per_hour: 0` to 20 silently turns "cancel nothing" into
// "cancel fifty times an hour".
type PolicyConfig struct {
	Mode             *string   `yaml:"mode"`
	Poll             *Duration `yaml:"poll"`
	CancelCapPerHour *int      `yaml:"cancel_cap_per_hour"`
	GraceSeconds     *int      `yaml:"grace_seconds"`
}

func (p PolicyConfig) ModeValue() string {
	if p.Mode == nil {
		return "dry-run"
	}
	return *p.Mode
}

func (p PolicyConfig) PollEvery() time.Duration {
	if p.Poll == nil {
		return 30 * time.Second
	}
	return p.Poll.Std()
}

func (p PolicyConfig) CancelCap() int {
	if p.CancelCapPerHour == nil {
		return 20
	}
	return *p.CancelCapPerHour
}

func (p PolicyConfig) Grace() time.Duration {
	if p.GraceSeconds == nil {
		return 45 * time.Second
	}
	return time.Duration(*p.GraceSeconds) * time.Second
}

type PoolConfig struct {
	StateDir string `yaml:"state_dir"`
	WorkDir  string `yaml:"work_dir"`
	// RunnerDir is an extracted actions-runner release. runlet copies it into
	// each lane; it never writes to the source.
	RunnerDir string `yaml:"runner_dir"`
	// RunnerURL is the registration scope: https://github.com/owner or
	// https://github.com/owner/repo.
	RunnerURL    string       `yaml:"runner_url"`
	MemoryBudget Bytes        `yaml:"memory_budget"`
	Lanes        []LaneConfig `yaml:"lanes"`
	// StaleLaneMinutes is how long a lane may report busy while GitHub has no
	// in-progress job for its runner before the supervisor stops the scope and
	// kills the process tree. Absent takes 15. An explicit 0 turns it off.
	StaleLaneMinutes *int `yaml:"stale_lane_minutes,omitempty"`
	// Scratch is the shared temp directory that build tools write to.
	Scratch ScratchConfig `yaml:"scratch,omitempty"`
}

// ScratchConfig watches and trims the temp directory builds write to. A host
// whose /tmp is a tmpfs spends memory on leftover build directories, and the
// next build then fails with a full filesystem.
type ScratchConfig struct {
	Path        string    `yaml:"path,omitempty"`
	Patterns    []string  `yaml:"patterns,omitempty"`
	Stale       *Duration `yaml:"stale,omitempty"`
	Every       *Duration `yaml:"every,omitempty"`
	WarnPercent *int      `yaml:"warn_percent,omitempty"`
}

func (s ScratchConfig) PathValue() string {
	if s.Path == "" {
		return "/tmp"
	}
	return s.Path
}

// PatternValues lists the directory names a sweep may remove. The default is
// what the go command leaves behind after an interrupted build.
func (s ScratchConfig) PatternValues() []string {
	if len(s.Patterns) == 0 {
		return []string{"go-build*"}
	}
	return s.Patterns
}

func (s ScratchConfig) StaleFor() time.Duration {
	if s.Stale == nil {
		return 12 * time.Hour
	}
	return s.Stale.Std()
}

// EveryFor is how often a sweep runs. Zero turns the sweep off; the usage
// gauge and the warning keep working.
func (s ScratchConfig) EveryFor() time.Duration {
	if s.Every == nil {
		return 24 * time.Hour
	}
	return s.Every.Std()
}

func (s ScratchConfig) WarnRatio() float64 {
	if s.WarnPercent == nil {
		return 0.85
	}
	return float64(*s.WarnPercent) / 100
}

// validate refuses a sweep that could remove more than it should. The sweep
// deletes directories, so a wrong path or a catch-all pattern costs real data.
func (s ScratchConfig) validate(workDir, stateDir string) []error {
	var errs []error
	path := filepath.Clean(s.PathValue())
	if !filepath.IsAbs(path) {
		errs = append(errs, fmt.Errorf("pool.scratch.path %q must be absolute", s.PathValue()))
	} else {
		for _, own := range []string{workDir, stateDir} {
			if own == "" {
				continue
			}
			own = filepath.Clean(own)
			if path == own || strings.HasPrefix(path, own+string(filepath.Separator)) {
				errs = append(errs, fmt.Errorf("pool.scratch.path %q must not be inside %q: the sweep would delete lane state", path, own))
			}
		}
	}
	if s.StaleFor() <= 0 {
		errs = append(errs, errors.New("pool.scratch.stale must be positive"))
	}
	if s.EveryFor() < 0 {
		errs = append(errs, errors.New("pool.scratch.every must not be negative"))
	}
	if s.WarnPercent != nil && (*s.WarnPercent <= 0 || *s.WarnPercent > 100) {
		errs = append(errs, fmt.Errorf("pool.scratch.warn_percent must be between 1 and 100, got %d", *s.WarnPercent))
	}
	if s.EveryFor() > 0 {
		if len(s.PatternValues()) == 0 {
			errs = append(errs, errors.New("pool.scratch.patterns must not be empty"))
		}
		for _, p := range s.PatternValues() {
			if p == "*" {
				errs = append(errs, errors.New(`pool.scratch.patterns must not contain "*": the sweep would remove every directory in the path`))
				continue
			}
			if _, err := filepath.Match(p, "probe"); err != nil {
				errs = append(errs, fmt.Errorf("pool.scratch.patterns %q is not a valid pattern: %w", p, err))
			}
		}
	}
	return errs
}

// StaleAfter is how long a lane may stay busy while GitHub has no in-progress
// job for its runner. Zero turns the watchdog off.
func (p PoolConfig) StaleAfter() time.Duration {
	if p.StaleLaneMinutes == nil {
		return 15 * time.Minute
	}
	return time.Duration(*p.StaleLaneMinutes) * time.Minute
}

type LaneConfig struct {
	Name       string   `yaml:"name"`
	Labels     []string `yaml:"labels"`
	Memory     Bytes    `yaml:"memory"`
	MemoryHigh Bytes    `yaml:"memory_high"`
	CPUQuota   string   `yaml:"cpu_quota"`
}

type RepoConfig struct {
	Slug             string `yaml:"slug"`
	CancelSuperseded bool   `yaml:"cancel_superseded"`
}

var slugRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var slugPartRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// laneNameRE keeps a lane name a plain directory name. It must start with an
// alphanumeric, which rejects ".", "..", and "./lane-0".
var laneNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// loopbackHosts is an allowlist, not a check for a private range. A LAN address
// is still reachable by every device on the network.
var loopbackHosts = map[string]bool{
	"127.0.0.1": true,
	"::1":       true,
	"localhost": true,
}

// Load reads path, rejects unknown keys, and validates. A returned config is
// safe to run; nothing is filled in later.
func Load(path string) (*Config, error) {
	// The config is not just data: runner_dir names the scripts runlet
	// executes, and lane names become paths it removes. Another user who can
	// write it owns the service, so it gets the same permission check as the
	// token file.
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("config %s must be mode 0600 (is %04o)", path, perm)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("parse %s: multiple YAML documents are not supported", path)
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	return &c, nil
}

// Validate collects every problem instead of stopping at the first: one run of
// the tool should tell the operator everything that is wrong.
func (c *Config) Validate() error {
	var errs []error

	host, _, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		errs = append(errs, fmt.Errorf("server.listen %q must be host:port: %w", c.Server.Listen, err))
	} else if !loopbackHosts[host] {
		errs = append(errs, fmt.Errorf("server.listen %q must be a loopback address (127.0.0.1 or [::1]); /metrics and /status have no authentication", c.Server.Listen))
	}

	switch c.Policy.ModeValue() {
	case "dry-run", "apply":
	default:
		errs = append(errs, fmt.Errorf("policy.mode must be dry-run or apply, got %q", c.Policy.ModeValue()))
	}
	if c.Policy.PollEvery() <= 0 {
		errs = append(errs, errors.New("policy.poll must be positive"))
	}
	if c.Policy.CancelCap() <= 0 {
		errs = append(errs, fmt.Errorf("policy.cancel_cap_per_hour must be positive, got %d", c.Policy.CancelCap()))
	}
	if c.Policy.Grace() < 0 {
		errs = append(errs, fmt.Errorf("policy.grace_seconds must not be negative, got %s", c.Policy.Grace()))
	}

	if len(c.Repos) == 0 {
		errs = append(errs, errors.New("repos must not be empty"))
	}
	seen := make(map[string]bool, len(c.Repos))
	for i, r := range c.Repos {
		if !slugRE.MatchString(r.Slug) {
			errs = append(errs, fmt.Errorf("repos[%d].slug %q must look like owner/name", i, r.Slug))
		}
		if seen[r.Slug] {
			errs = append(errs, fmt.Errorf("repos[%d].slug %q is listed twice", i, r.Slug))
		}
		seen[r.Slug] = true
	}

	var total Bytes
	laneNames := make(map[string]bool, len(c.Pool.Lanes))
	for i, l := range c.Pool.Lanes {
		if strings.TrimSpace(l.Name) == "" {
			errs = append(errs, fmt.Errorf("pool.lanes[%d].name is required", i))
		} else if !laneNameRE.MatchString(l.Name) {
			// The name becomes a directory under work_dir and state_dir, and
			// Prepare removes paths inside it. ".." or a separator here would
			// delete files outside the pool.
			errs = append(errs, fmt.Errorf("pool.lanes[%d].name %q must match %s", i, l.Name, laneNameRE))
		}
		if laneNames[l.Name] {
			errs = append(errs, fmt.Errorf("pool.lanes[%d].name %q is listed twice", i, l.Name))
		}
		laneNames[l.Name] = true
		if len(l.Labels) == 0 {
			errs = append(errs, fmt.Errorf("pool.lanes[%d].labels must not be empty", i))
		}
		if l.Memory <= 0 {
			errs = append(errs, fmt.Errorf("pool.lanes[%d].memory must be positive", i))
		}
		if l.MemoryHigh > l.Memory {
			errs = append(errs, fmt.Errorf("pool.lanes[%d].memory_high must not exceed memory", i))
		}
		total += l.Memory
	}

	if len(c.Pool.Lanes) > 0 {
		if strings.TrimSpace(c.Pool.StateDir) == "" {
			errs = append(errs, errors.New("pool.state_dir is required when lanes are configured"))
		}
		if strings.TrimSpace(c.Pool.WorkDir) == "" {
			errs = append(errs, errors.New("pool.work_dir is required when lanes are configured"))
		}
		if strings.TrimSpace(c.Pool.RunnerDir) == "" {
			errs = append(errs, errors.New("pool.runner_dir is required when lanes are configured"))
		} else if err := checkRunnerDir(c.Pool.RunnerDir); err != nil {
			errs = append(errs, err)
		}
		if strings.TrimSpace(c.Pool.RunnerURL) == "" {
			errs = append(errs, errors.New("pool.runner_url is required when lanes are configured"))
		} else if _, err := ParseScope(c.Pool.RunnerURL); err != nil {
			errs = append(errs, err)
		}
		if c.Pool.MemoryBudget <= 0 {
			errs = append(errs, errors.New("pool.memory_budget is required when lanes are configured"))
		} else if total > c.Pool.MemoryBudget {
			errs = append(errs, fmt.Errorf("lane memory %s exceeds pool.memory_budget %s", total, c.Pool.MemoryBudget))
		}
		if c.Pool.StaleLaneMinutes != nil && *c.Pool.StaleLaneMinutes < 0 {
			errs = append(errs, fmt.Errorf("pool.stale_lane_minutes must not be negative, got %d", *c.Pool.StaleLaneMinutes))
		}
		errs = append(errs, c.Pool.Scratch.validate(c.Pool.WorkDir, c.Pool.StateDir)...)
	}

	return errors.Join(errs...)
}

// checkRunnerDir proves the directory is an extracted runner before a lane
// tries to run it. A missing config.sh is a confusing failure 30 seconds later.
func checkRunnerDir(dir string) error {
	for _, name := range []string{"config.sh", "run.sh", filepath.Join("bin", "Runner.Listener")} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("pool.runner_dir %s: %w", dir, err)
		}
	}
	return nil
}

// CheckRunnerDir is checkRunnerDir for `runlet doctor`.
func CheckRunnerDir(dir string) error { return checkRunnerDir(dir) }

// Scope is an organization or repository that owns runners.
type Scope struct {
	Owner string
	Repo  string // empty for an organization scope
}

// ParseScope accepts https://github.com/owner, https://github.com/owner/repo,
// and the bare owner or owner/repo forms. The host is pinned: the registration
// token is always minted against api.github.com, so handing a different host to
// config.sh would post that token to whoever owns the name.
func ParseScope(raw string) (Scope, error) {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return Scope{}, fmt.Errorf("pool.runner_url %q: %w", raw, err)
		}
		if u.Scheme != "https" {
			return Scope{}, fmt.Errorf("pool.runner_url %q must use https", raw)
		}
		if u.User != nil {
			return Scope{}, fmt.Errorf("pool.runner_url %q must not carry credentials", raw)
		}
		if u.Hostname() != "github.com" || u.Port() != "" {
			return Scope{}, fmt.Errorf("pool.runner_url %q must point at github.com", raw)
		}
		s = strings.Trim(u.Path, "/")
	}
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 1:
		if !slugPartRE.MatchString(parts[0]) {
			return Scope{}, fmt.Errorf("pool.runner_url %q: %q is not an owner name", raw, parts[0])
		}
		return Scope{Owner: parts[0]}, nil
	case 2:
		if !slugPartRE.MatchString(parts[0]) || !slugPartRE.MatchString(parts[1]) {
			return Scope{}, fmt.Errorf("pool.runner_url %q: want owner/repo", raw)
		}
		return Scope{Owner: parts[0], Repo: parts[1]}, nil
	default:
		return Scope{}, fmt.Errorf("pool.runner_url %q: want https://github.com/owner or https://github.com/owner/repo", raw)
	}
}

func (s Scope) String() string {
	if s.Repo == "" {
		return s.Owner
	}
	return s.Owner + "/" + s.Repo
}

// URL is what config.sh --url receives. It is rebuilt from the parsed scope so
// the value handed to the runner cannot differ from the value that was
// validated.
func (s Scope) URL() string {
	if s.Repo == "" {
		return "https://github.com/" + s.Owner
	}
	return "https://github.com/" + s.Owner + "/" + s.Repo
}

// APIPath is the REST prefix for this scope's runners and registration tokens.
func (s Scope) APIPath() string {
	if s.Repo == "" {
		return "/orgs/" + s.Owner + "/actions/runners"
	}
	return "/repos/" + s.Owner + "/" + s.Repo + "/actions/runners"
}
