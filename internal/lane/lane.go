// Package lane owns one runner process: its directory tree, its registration,
// its systemd scope, and its cleanup. It never decides when to start; the
// supervisor does that.
package lane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jungley8/runlet/internal/model"
)

// Command is one external process. Tests assert on it instead of running it.
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// String renders the command for a log line. config.sh needs the registration
// token on its command line, so the token is replaced here: this string ends up
// in error messages and in the journal.
func (c Command) String() string {
	args := make([]string, len(c.Args))
	copy(args, c.Args)
	for i := range args {
		if args[i] == "--token" && i+1 < len(args) {
			args[i+1] = "<redacted>"
		}
		if strings.HasPrefix(args[i], "--token=") {
			args[i] = "--token=<redacted>"
		}
	}
	return c.Path + " " + strings.Join(args, " ")
}

// Execer runs external commands. The real one is OS; tests use a fake.
type Execer interface {
	Run(ctx context.Context, cmd Command) error
	Output(ctx context.Context, cmd Command) (string, error)
}

// OS executes real processes.
type OS struct{}

func (OS) Run(ctx context.Context, c Command) error {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The output of config.sh and run.sh is the only diagnostic there is.
		// It never contains the registration token.
		return fmt.Errorf("%s: %w: %s", c.String(), err, tail(string(out), 800))
	}
	return nil
}

func (OS) Output(ctx context.Context, c Command) (string, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", c.String(), err)
	}
	return string(out), nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// Options configures one lane.
type Options struct {
	Index      int
	Name       string
	Labels     []string
	Memory     int64
	MemoryHigh int64
	CPUQuota   string
	RunnerDir  string // source install; read-only
	RunnerURL  string // registration scope
	Dir        string // <work_dir>/<name>
	StateDir   string // <state_dir>/lanes/<name>
	CacheDir   string // <work_dir>/cache
	Host       string // short hostname, part of the runner name
	Exec       Execer
	Log        *slog.Logger
}

// Lane is one runner slot. Its methods are safe for concurrent use.
type Lane struct {
	opts Options

	mu         sync.Mutex
	attempt    string
	unit       string
	runnerName string
	startedAt  time.Time
}

// State is what the status endpoint and a restarted supervisor need.
type State struct {
	Unit       string    `json:"unit"`
	RunnerName string    `json:"runner_name"`
	Attempt    string    `json:"attempt"`
	StartedAt  time.Time `json:"started_at"`
}

func New(opts Options) *Lane {
	if opts.Exec == nil {
		opts.Exec = OS{}
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Host == "" {
		opts.Host = "host"
	}
	return &Lane{opts: opts}
}

func (l *Lane) Name() string { return l.opts.Name }
func (l *Lane) Index() int   { return l.opts.Index }

// Current returns the running attempt, or the zero State when idle.
func (l *Lane) Current() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attempt == "" {
		return State{}
	}
	return State{Unit: l.unit, RunnerName: l.runnerName, Attempt: l.attempt, StartedAt: l.startedAt}
}

// Prepare creates the lane's private directory tree. Each lane needs its own
// copy: .runner, .credentials and _work are not safe to share between runner
// processes. Files are hardlinked, so the copy costs almost no disk.
func (l *Lane) Prepare() error {
	if err := os.MkdirAll(l.opts.Dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(l.homeDir(), 0o755); err != nil {
		return err
	}
	for _, d := range []string{l.opts.CacheDir + "/go-build", l.opts.CacheDir + "/go-mod", l.opts.CacheDir + "/pnpm", l.opts.StateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return l.syncRunnerTree()
}

func (l *Lane) homeDir() string   { return filepath.Join(l.opts.Dir, "home") }
func (l *Lane) statePath() string { return filepath.Join(l.opts.StateDir, "lane.json") }

// syncRunnerTree hardlinks the runner install into the lane. A version change
// in the source wipes the lane copy first, because a half-upgraded runner
// fails in ways that look like flaky jobs.
func (l *Lane) syncRunnerTree() error {
	version, _ := os.ReadFile(filepath.Join(l.opts.RunnerDir, "version"))
	stamp := strings.TrimSpace(string(version))
	markerPath := filepath.Join(l.opts.Dir, ".runlet-runner-version")
	if old, err := os.ReadFile(markerPath); err == nil && strings.TrimSpace(string(old)) == stamp && stamp != "" {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(l.opts.Dir, "bin")); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(l.opts.Dir, "externals")); err != nil {
		return err
	}
	for _, name := range []string{"config.sh", "run.sh"} {
		if err := copyFile(filepath.Join(l.opts.RunnerDir, name), filepath.Join(l.opts.Dir, name)); err != nil {
			return err
		}
	}
	for _, name := range []string{"bin", "externals"} {
		if err := linkTree(filepath.Join(l.opts.RunnerDir, name), filepath.Join(l.opts.Dir, name)); err != nil {
			return err
		}
	}
	return os.WriteFile(markerPath, []byte(stamp+"\n"), 0o644)
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, info.Mode().Perm())
}

// linkTree hardlinks every regular file. A hardlink shares one inode: the lane
// cannot corrupt it, and no disk is spent. Filesystems without hardlinks fall
// back to a copy.
func linkTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.Link(path, target); err == nil {
			return nil
		}
		return copyFile(path, target)
	})
}

// Attempt starts a new attempt identity. The name is unique per attempt:
// reusing a stable name with --replace lets two overlapping attempts
// de-register each other.
func (l *Lane) Attempt() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		// A clock-based suffix is still unique enough to avoid the collision
		// this exists to prevent.
		copy(buf, []byte(strconv.FormatInt(time.Now().UnixNano(), 36)))
	}
	l.attempt = hex.EncodeToString(buf)[:8]
	l.unit = fmt.Sprintf("runlet-lane-%d-%s", l.opts.Index, l.attempt)
	l.runnerName = fmt.Sprintf("runlet-%s-lane-%d-%s", l.opts.Host, l.opts.Index, l.attempt)
	l.startedAt = time.Now()
	return State{Unit: l.unit, RunnerName: l.runnerName, Attempt: l.attempt, StartedAt: l.startedAt}
}

// SaveState writes the attempt to disk so a restarted supervisor can adopt it.
func (l *Lane) SaveState(s State) error {
	if err := os.MkdirAll(l.opts.StateDir, 0o755); err != nil {
		return err
	}
	return writeJSON(l.statePath(), s)
}

// LoadState reads a previous attempt, if any.
func (l *Lane) LoadState() (State, error) {
	var s State
	if err := readJSON(l.statePath(), &s); err != nil {
		return State{}, err
	}
	return s, nil
}

// ClearState forgets the attempt. It runs after cleanup, never before: the
// state file is how a crashed supervisor finds its orphans.
func (l *Lane) ClearState() error {
	err := os.Remove(l.statePath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Register runs config.sh with a one-use token. The caller mints the token
// immediately before this call and must not cache it.
func (l *Lane) Register(ctx context.Context, tok model.RegistrationToken, name string) error {
	args := []string{
		"--unattended",
		"--ephemeral",
		"--name", name,
		"--labels", strings.Join(l.opts.Labels, ","),
		"--url", l.opts.RunnerURL,
		"--work", "_work",
		"--token", tok.Token,
	}
	err := l.opts.Exec.Run(ctx, Command{
		Path: filepath.Join(l.opts.Dir, "config.sh"),
		Args: args,
		Env:  l.env(),
		Dir:  l.opts.Dir,
	})
	if err != nil {
		// config.sh output is safe to log; the arguments are not.
		return fmt.Errorf("register runner %s: %w", name, err)
	}
	return nil
}

// Scope is the systemd-run command that starts the runner under a cgroup.
func (l *Lane) Scope(unit string) Command {
	args := []string{
		"--user",
		"--scope",
		"--quiet",
		"--collect",
		"--unit=" + unit,
		"--property=MemoryMax=" + strconv.FormatInt(l.opts.Memory, 10),
		"--property=KillMode=control-group",
		"--property=TimeoutStopSec=20",
	}
	if l.opts.MemoryHigh > 0 {
		args = append(args, "--property=MemoryHigh="+strconv.FormatInt(l.opts.MemoryHigh, 10))
	}
	if l.opts.CPUQuota != "" {
		args = append(args, "--property=CPUQuota="+l.opts.CPUQuota)
	}
	args = append(args,
		"--setenv=HOME="+l.homeDir(),
		"--setenv=GOCACHE="+filepath.Join(l.opts.CacheDir, "go-build"),
		"--setenv=GOMODCACHE="+filepath.Join(l.opts.CacheDir, "go-mod"),
		"--setenv=PNPM_STORE_DIR="+filepath.Join(l.opts.CacheDir, "pnpm"),
		"--working-directory="+l.opts.Dir,
		filepath.Join(l.opts.Dir, "run.sh"),
	)
	return Command{Path: "systemd-run", Args: args, Dir: l.opts.Dir, Env: l.env()}
}

// Stop stops the scope. systemd kills the whole cgroup, so a job's child
// processes cannot outlive the lane.
func (l *Lane) Stop(ctx context.Context, unit string) error {
	if unit == "" {
		return nil
	}
	err := l.opts.Exec.Run(ctx, Command{Path: "systemctl", Args: []string{"--user", "stop", unit}})
	return err
}

// IsActive reports whether a unit still runs. Adoption uses it.
func (l *Lane) IsActive(ctx context.Context, unit string) bool {
	_, err := l.opts.Exec.Output(ctx, Command{
		Path: "systemctl",
		Args: []string{"--user", "is-active", "--quiet", unit},
	})
	return err == nil
}

// ListUnits returns the unit names of every lane scope, active or not.
func (l *Lane) ListUnits(ctx context.Context) ([]string, error) {
	out, err := l.opts.Exec.Output(ctx, Command{
		Path: "systemctl",
		Args: []string{"--user", "list-units", "--type=scope", "--all", "--no-legend", "--plain", "--no-pager", "runlet-lane-*"},
	})
	if err != nil {
		return nil, err
	}
	var units []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasPrefix(fields[0], "runlet-lane-") {
			units = append(units, strings.TrimSuffix(fields[0], ".scope"))
		}
	}
	return units, nil
}

// Cleanup removes what a job leaves behind. It keeps _work/_actions: that is
// the per-lane action cache, and dropping it makes every job re-download every
// action.
func (l *Lane) Cleanup(ctx context.Context) {
	work := filepath.Join(l.opts.Dir, "_work")
	entries, err := os.ReadDir(work)
	if err == nil {
		for _, e := range entries {
			if e.Name() == "_actions" {
				continue
			}
			if err := os.RemoveAll(filepath.Join(work, e.Name())); err != nil {
				l.opts.Log.Warn("lane cleanup", "lane", l.opts.Name, "path", e.Name(), "err", err)
			}
		}
	}
	rotateDiag(filepath.Join(l.opts.Dir, "_diag"), 7*24*time.Hour)
}

func rotateDiag(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// Deregister is done by the supervisor through the API: it needs the list and
// delete endpoints of the registration scope, which this package does not own.

func (l *Lane) env() []string {
	env := []string{
		"HOME=" + l.homeDir(),
		"PATH=" + os.Getenv("PATH"),
		"LANG=C.UTF-8",
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}
