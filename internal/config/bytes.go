package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Bytes is a size parsed from strings such as "512MiB", "1.5GiB", or "2GB".
// Both binary (KiB) and decimal (KB) suffixes are accepted because operators
// write both and silently treating them the same would be wrong.
type Bytes int64

func (b Bytes) Int64() int64 { return int64(b) }

func (b Bytes) String() string {
	f := float64(b)
	for _, u := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if f < 1024 || u == "TiB" {
			return fmt.Sprintf("%.1f%s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%dB", int64(b))
}

var bytesRE = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*(b|k|kb|kib|m|mb|mib|g|gb|gib|t|tb|tib)?$`)

// ParseBytes parses a human size. An empty suffix is bytes.
func ParseBytes(s string) (Bytes, error) {
	s = strings.TrimSpace(s)
	m := bytesRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid size %q: use 512MiB, 1.5GiB, or 2GB", s)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	var mult float64
	switch strings.ToLower(m[2]) {
	case "", "b":
		mult = 1
	case "k", "kb":
		mult = 1e3
	case "kib":
		mult = 1024
	case "m", "mb":
		mult = 1e6
	case "mib":
		mult = 1024 * 1024
	case "g", "gb":
		mult = 1e9
	case "gib":
		mult = 1024 * 1024 * 1024
	case "t", "tb":
		mult = 1e12
	case "tib":
		mult = 1024 * 1024 * 1024 * 1024
	}
	out := v * mult
	if out > math.MaxInt64 {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return Bytes(int64(out)), nil
}

func (b *Bytes) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err == nil {
		v, err := ParseBytes(s)
		if err != nil {
			return err
		}
		*b = v
		return nil
	}
	var n int64
	if err := node.Decode(&n); err == nil {
		*b = Bytes(n)
		return nil
	}
	return errors.New(`size must be a string like "1.5GiB" or an integer number of bytes`)
}

// Duration accepts "30s" or an integer number of seconds.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }
func (d Duration) String() string     { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("duration %q: %w", s, err)
		}
		*d = Duration(v)
		return nil
	}
	var secs int64
	if err := node.Decode(&secs); err != nil {
		return errors.New(`duration must be a string like "30s" or an integer number of seconds`)
	}
	*d = Duration(time.Duration(secs) * time.Second)
	return nil
}
