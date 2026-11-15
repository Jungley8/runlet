package scratch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkdir(t *testing.T, dir string, age time.Duration, now time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	when := now.Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSweepRemovesOnlyStaleMatchingDirectories is the whole guard: a build
// leaves a work directory behind, and the sweep must take that one and nothing
// else. The path is shared with everything else on the host.
func TestSweepRemovesOnlyStaleMatchingDirectories(t *testing.T) {
	now := time.Now()
	root := t.TempDir()

	stale := mkdir(t, filepath.Join(root, "go-build111"), 13*time.Hour, now)
	fresh := mkdir(t, filepath.Join(root, "go-build222"), time.Hour, now)
	other := mkdir(t, filepath.Join(root, "keep-me"), 13*time.Hour, now)
	file := filepath.Join(root, "go-build333")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-13 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "go-build444")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	// Chtimes follows the link, so this changes the target, not the symlink:
	// the symlink stays fresh and is skipped by age. The symlink branch below
	// is kept as defence in depth, not as the only barrier.

	removed, err := Sweep(root, []string{"go-build*"}, 12*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the stale build directory survived")
	}
	for _, path := range []string{fresh, other} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s must survive: %v", path, err)
		}
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("a plain file must survive: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("a symlink must survive: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("the symlink target must survive: %v", err)
	}
}

func TestSweepWithoutPatternsRefuses(t *testing.T) {
	if _, err := Sweep(t.TempDir(), nil, time.Hour, time.Now()); err == nil {
		t.Fatal("an empty pattern list must be an error, not a wildcard")
	}
}

func TestUsageOfReportsARealFilesystem(t *testing.T) {
	u, err := UsageOf(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if u.Total == 0 {
		t.Fatalf("total = 0; statfs returned nothing")
	}
	if u.Used > u.Total {
		t.Fatalf("used %d exceeds total %d", u.Used, u.Total)
	}
}
