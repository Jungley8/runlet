package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lock takes an exclusive lock on <state_dir>/runlet.lock. Two supervisors
// would register two runners for the same lane, and GitHub would hand work to
// both.
func (s *Supervisor) lock() (func(), error) {
	stateDir := s.opts.Config.Pool.StateDir
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	path := filepath.Join(stateDir, "runlet.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another runlet supervisor holds %s", path)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock file: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
