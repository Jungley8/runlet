// Package scratch reports and trims the shared temp directory that builds
// write to. On a host whose /tmp is a tmpfs, an interrupted build leaves its
// work directory behind, and the tmpfs is memory: a few of them fill both the
// filesystem and the RAM.
package scratch

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Usage is the size of the filesystem that holds a path, and how much of it is
// in use.
type Usage struct {
	Used  uint64
	Total uint64
}

// Ratio is the used fraction, 0 when the filesystem reports no size.
func (u Usage) Ratio() float64 {
	if u.Total == 0 {
		return 0
	}
	return float64(u.Used) / float64(u.Total)
}

// UsageOf reads the filesystem statistics of path.
func UsageOf(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, err
	}
	bsize := uint64(st.Bsize)
	return Usage{
		Used:  (st.Blocks - st.Bfree) * bsize,
		Total: st.Blocks * bsize,
	}, nil
}

// Sweep removes matching directories under path that are older than olderThan
// and owned by this user. It refuses to follow a symlink, to touch a plain
// file, or to remove anything another user owns.
func Sweep(path string, patterns []string, olderThan time.Duration, now time.Time) (int, error) {
	if len(patterns) == 0 {
		return 0, fmt.Errorf("scratch: no patterns")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-olderThan)
	removed := 0
	for _, e := range entries {
		if !matches(e.Name(), patterns) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if !ownedByUs(info) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(path, e.Name())); err != nil {
			continue
		}
		removed++
	}
	return removed, nil
}

func matches(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := filepath.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

func ownedByUs(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Getuid()
}
