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
	"slices"
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

// waitDelay bounds the wait for stdout after the direct child exits. A lane is
// started through systemd-run, and the runner inherits the pipe. Killing
// systemd-run does not close it: the scope keeps running on purpose, so without
// this bound a supervisor stop blocks until the job ends and systemd gives up
// and SIGKILLs the whole thing.
const waitDelay = 5 * time.Second

func (OS) Run(ctx context.Context, c Command) error {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.WaitDelay = waitDelay
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
	cmd.WaitDelay = waitDelay
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
	Env        map[string]string
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

// runnerStateFiles are per-install state, never part of a lane copy: config.sh
// refuses to run while .runner or its pre-migration name exists, and .env/.path
// describe the source install's user and PATH. config.sh writes both again.
var runnerStateFiles = map[string]bool{
	".runner":                true,
	".runner_migrated":       true,
	".credentials":           true,
	".credentials_migrated":  true,
	".credentials_rsaparams": true,
	".env":                   true,
	".path":                  true,
	".service":               true,
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
	if err := ensureRealDir(l.tmpDir()); err != nil {
		return err
	}
	if err := clearDir(l.tmpDir()); err != nil {
		return err
	}
	for _, d := range []string{l.opts.CacheDir + "/go-build", l.opts.CacheDir + "/go-mod", l.opts.CacheDir + "/pnpm", l.opts.StateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	// The lane directory is job-writable. A job can replace any of these with a
	// symlink to somewhere else, and the next attempt would write through it.
	for _, d := range []string{l.opts.Dir, l.homeDir(), filepath.Join(l.opts.Dir, "_work")} {
		if err := ensureRealDir(d); err != nil {
			return err
		}
	}
	if err := l.clearStaleRegistration(); err != nil {
		return err
	}
	return l.syncRunnerTree()
}

// ensureRealDir makes path a real directory owned by us. A symlink or a plain
// file at that name is removed first: following it would put job data, or the
// supervisor's writes, outside the lane.
func ensureRealDir(path string) error {
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return os.MkdirAll(path, 0o700)
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		return os.MkdirAll(path, 0o700)
	}
	return nil
}

// tmpDir is the lane's private TMPDIR. It lives on disk with the lane, so a
// leftover build directory costs disk instead of the host's memory.
func (l *Lane) tmpDir() string { return filepath.Join(l.opts.Dir, "tmp") }

// clearDir removes the entries of a real directory. A symlink at the path is
// replaced, not followed: a job can plant one and make the next attempt delete
// its target.
func clearDir(path string) error {
	if err := ensureRealDir(path); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(path, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// clearStaleRegistration drops the previous attempt's identity. config.sh
// refuses to run while .runner exists ("already configured"), and every
// interrupted attempt leaves one behind: the supervisor is killed, not the
// runner. Each attempt registers under a fresh name, so the old one is dead.
// RemoveAll, not Remove: a job can leave a directory at one of these names and
// wedge the lane forever.
func (l *Lane) clearStaleRegistration() error {
	for name := range runnerStateFiles {
		if err := os.RemoveAll(filepath.Join(l.opts.Dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func (l *Lane) homeDir() string   { return filepath.Join(l.opts.Dir, "home") }
func (l *Lane) statePath() string { return filepath.Join(l.opts.StateDir, "lane.json") }

// isLaunchFile reports whether a top-level file belongs to the runner program.
// The release ships shell scripts and templates; an install also collects
// tarballs, backups and other leftovers that must not reach every lane.
func isLaunchFile(name string) bool {
	switch filepath.Ext(name) {
	case ".sh", ".cmd", ".template":
		return true
	}
	return name == "version"
}

// syncRunnerTree copies the runner install into the lane. Launch files are
// rewritten on every attempt: they are small, and a job owns the lane
// directory between attempts. A tampered config.sh would otherwise be executed
// by the supervisor itself, outside the lane's resource caps. bin and externals
// are hardlinked once per runner version.
func (l *Lane) syncRunnerTree() error {
	// Every launch file is part of the install: run.sh needs
	// run-helper.sh.template and safe_sleep.sh next to itself, and a missing
	// helper makes the runner exit 0 before it ever listens.
	entries, err := os.ReadDir(l.opts.RunnerDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || runnerStateFiles[e.Name()] || !isLaunchFile(e.Name()) {
			continue
		}
		src := filepath.Join(l.opts.RunnerDir, e.Name())
		if err := copyFile(src, filepath.Join(l.opts.Dir, e.Name())); err != nil {
			return err
		}
	}

	stamp := l.sourceStamp()
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
	for _, name := range []string{"bin", "externals"} {
		if err := linkTree(filepath.Join(l.opts.RunnerDir, name), filepath.Join(l.opts.Dir, name)); err != nil {
			return err
		}
	}
	return os.WriteFile(markerPath, []byte(stamp+"\n"), 0o644)
}

// maxLaunchFile bounds what the supervisor will read from a job-writable
// directory. A 10 GiB "version" file would otherwise OOM it.
const maxLaunchFile = 4 << 20

// sourceStamp identifies the runner build. The official layout has a version
// file; an install that came from the tarball may not, so the listener's size
// and mtime stand in for it.
func (l *Lane) sourceStamp() string {
	if raw, err := os.ReadFile(filepath.Join(l.opts.RunnerDir, "version")); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v
		}
	}
	info, err := os.Stat(filepath.Join(l.opts.RunnerDir, "bin", "Runner.Listener"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().Unix())
}

func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	if info.Size() > maxLaunchFile {
		return fmt.Errorf("%s is %d bytes, over the %d byte limit", src, info.Size(), maxLaunchFile)
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// The destination is in a job-writable directory. Writing through a
	// planted symlink would clobber whatever it points at.
	if old, err := os.Lstat(dst); err == nil && (old.Mode()&os.ModeSymlink != 0 || !old.Mode().IsRegular()) {
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
	}
	return os.WriteFile(dst, raw, info.Mode().Perm())
}

// linkTree hardlinks every regular file. A hardlink shares one inode: the lane
// cannot corrupt it, and no disk is spent. Filesystems without hardlinks fall
// back to a copy. The installed runner keeps bin and externals as symlinks to
// versioned directories, so the root is resolved first.
func linkTree(src, dst string) error {
	root, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
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
		// Swap counts against nothing otherwise: MemoryMax caps RAM only, so a
		// runaway job swaps the whole box instead of dying in its own cgroup.
		"--property=MemorySwapMax=0",
		"--property=TasksMax=2048",
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
		// TMPDIR is the lane's own directory, not the host's /tmp. A host whose
		// /tmp is a tmpfs loses RAM to every interrupted build, and one full
		// tmpfs fails every lane at once. The lane directory is on disk and is
		// emptied at the start of each attempt.
		"--setenv=TMPDIR="+l.tmpDir(),
	)
	for _, k := range l.sortedEnvKeys() {
		args = append(args, "--setenv="+k+"="+l.opts.Env[k])
	}
	args = append(args,
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
	err := l.opts.Exec.Run(ctx, Command{Path: "systemctl", Args: []string{"--user", "stop", scopeName(unit)}})
	return err
}

// scopeName is the unit name systemd stores. systemctl resolves a bare name to
// .service, so a scope has to be addressed with its suffix: "is-active
// runlet-lane-0-abcd" reports an inactive .service and exits 3.
func scopeName(unit string) string {
	if strings.HasSuffix(unit, ".scope") {
		return unit
	}
	return unit + ".scope"
}

// IsActive reports whether a unit still runs. Adoption uses it.
func (l *Lane) IsActive(ctx context.Context, unit string) bool {
	_, err := l.opts.Exec.Output(ctx, Command{
		Path: "systemctl",
		Args: []string{"--user", "is-active", "--quiet", scopeName(unit)},
	})
	return err == nil
}

// StartedAt returns when the unit became active, in microseconds since boot,
// so adoption can tell a stale scope from the one running the current job.
// Zero means "unknown".
func (l *Lane) StartedAt(ctx context.Context, unit string) int64 {
	out, err := l.opts.Exec.Output(ctx, Command{
		Path: "systemctl",
		Args: []string{"--user", "show", scopeName(unit), "--property=ActiveEnterTimestampMonotonic", "--value"},
	})
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return n
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
//
// Every path here is job-writable, and both ReadDir and RemoveAll follow a
// symlinked directory. A job that replaces _work with a link to $HOME would
// otherwise make the supervisor delete the user's home directory.
func (l *Lane) Cleanup(ctx context.Context) {
	work := filepath.Join(l.opts.Dir, "_work")
	if info, err := os.Lstat(work); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		entries, err := os.ReadDir(work)
		if err == nil {
			for _, e := range entries {
				if e.Name() == "_actions" {
					continue
				}
				path := filepath.Join(work, e.Name())
				if err := os.RemoveAll(path); err != nil {
					l.opts.Log.Warn("lane cleanup", "lane", l.opts.Name, "path", e.Name(), "err", err)
				}
			}
		}
	}
	rotateDiag(filepath.Join(l.opts.Dir, "_diag"), 7*24*time.Hour)
	if err := clearDir(l.tmpDir()); err != nil {
		l.opts.Log.Warn("lane cleanup: temp directory", "lane", l.opts.Name, "err", err)
	}
}

func rotateDiag(dir string, maxAge time.Duration) {
	// Same hazard as _work: a symlinked _diag would make this prune files
	// somewhere else.
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
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

func (l *Lane) sortedEnvKeys() []string {
	if len(l.opts.Env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(l.opts.Env))
	for k := range l.opts.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (l *Lane) env() []string {
	env := []string{
		"HOME=" + l.homeDir(),
		"PATH=" + os.Getenv("PATH"),
		"LANG=C.UTF-8",
	}
	for _, k := range []string{
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
		// systemd-run --user and systemctl --user find the user manager through
		// the runtime directory. Without it they fail with "Failed to connect to
		// bus: No medium found".
		"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
	} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, k := range l.sortedEnvKeys() {
		env = append(env, k+"="+l.opts.Env[k])
	}
	return env
}
