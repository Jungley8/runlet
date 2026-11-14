package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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
	StateDir     string       `yaml:"state_dir"`
	WorkDir      string       `yaml:"work_dir"`
	MemoryBudget Bytes        `yaml:"memory_budget"`
	Lanes        []LaneConfig `yaml:"lanes"`
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
		if c.Pool.MemoryBudget <= 0 {
			errs = append(errs, errors.New("pool.memory_budget is required when lanes are configured"))
		} else if total > c.Pool.MemoryBudget {
			errs = append(errs, fmt.Errorf("lane memory %s exceeds pool.memory_budget %s", total, c.Pool.MemoryBudget))
		}
	}

	return errors.Join(errs...)
}
