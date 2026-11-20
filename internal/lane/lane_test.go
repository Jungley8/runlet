package lane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jungley8/runlet/internal/model"
)

type fakeExec struct {
	commands []Command
	err      error
}

func (f *fakeExec) Run(_ context.Context, c Command) error {
	f.commands = append(f.commands, c)
	return f.err
}

func (f *fakeExec) Output(_ context.Context, c Command) (string, error) {
	f.commands = append(f.commands, c)
	return "", f.err
}

func testOptions(t *testing.T, exec Execer) Options {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	return Options{
		Index:      1,
		Name:       "lane-1",
		Labels:     []string{"self-hosted", "linux", "x64"},
		Memory:     1536 << 20,
		MemoryHigh: 1200 << 20,
		CPUQuota:   "250%",
		RunnerDir:  fakeRunnerDir(t),
		RunnerURL:  "https://github.com/acme",
		Dir:        filepath.Join(work, "lane-1"),
		StateDir:   filepath.Join(dir, "state", "lane-1"),
		CacheDir:   filepath.Join(work, "cache"),
		Host:       "box",
		Exec:       exec,
	}
}

// fakeRunnerDir is an extracted runner, reduced to the files runlet touches.
func fakeRunnerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"config.sh":              "#!/bin/sh\n",
		"run.sh":                 "#!/bin/sh\n",
		"run-helper.sh.template": "#!/bin/sh\n",
		"safe_sleep.sh":          "#!/bin/sh\n",
		"version":                "2.338.0\n",
		".runner":                "{\"agentId\":1}",
		".runner_migrated":       "{\"agentId\":1}",
		".credentials":           "{}",
		".path":                  "/root/bin\n",
		"actions-runner.tar.gz":  "backup",
		"bin/Runner.Listener":    "binary\n",
		"externals/node":         "runtime\n",
	}
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestScopeCommandCarriesTheLimits(t *testing.T) {
	l := New(testOptions(t, &fakeExec{}))
	cmd := l.Scope("runlet-lane-1-abcd1234")
	joined := strings.Join(cmd.Args, " ")

	for _, want := range []string{
		"--scope",
		"--unit=runlet-lane-1-abcd1234",
		"--property=MemoryMax=1610612736",
		"--property=MemoryHigh=1258291200",
		"--property=CPUQuota=250%",
		"--property=KillMode=control-group",
		"--setenv=GOCACHE=" + filepath.Join(l.opts.CacheDir, "go-build"),
		// TMPDIR is the lane's own directory: a shared tmpfs loses RAM to every
		// interrupted build, and one full tmpfs fails every lane at once.
		"--setenv=TMPDIR=" + filepath.Join(l.opts.Dir, "tmp"),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("command missing %q:\n%s", want, joined)
		}
	}
	if cmd.Args[len(cmd.Args)-1] != filepath.Join(l.opts.Dir, "run.sh") {
		t.Fatalf("last arg = %q, want run.sh", cmd.Args[len(cmd.Args)-1])
	}
	if cmd.Path != "systemd-run" {
		t.Fatalf("path = %q", cmd.Path)
	}
}

func TestAttemptIsUniquePerTry(t *testing.T) {
	l := New(testOptions(t, &fakeExec{}))
	a := l.Attempt()
	b := l.Attempt()
	if a.Attempt == b.Attempt || a.RunnerName == b.RunnerName || a.Unit == b.Unit {
		t.Fatalf("two attempts share an identity: %+v %+v", a, b)
	}
	if !strings.HasPrefix(a.RunnerName, "runlet-box-lane-1-") {
		t.Fatalf("runner name = %q", a.RunnerName)
	}
	if len(a.Attempt) != 8 {
		t.Fatalf("attempt suffix = %q, want 8 hex chars", a.Attempt)
	}
}

func TestPrepareHardlinksTheRunner(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(opts.RunnerDir, "bin", "Runner.Listener")
	dst := filepath.Join(opts.Dir, "bin", "Runner.Listener")
	si, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(si, di) {
		t.Fatal("lane copy is not a hardlink; disk use would double per lane")
	}
	for _, name := range []string{"config.sh", "run.sh"} {
		if _, err := os.Stat(filepath.Join(opts.Dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Preparing twice must not wipe the lane's own _work.
	work := filepath.Join(opts.Dir, "_work", "_actions")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("_work/_actions was removed by a second Prepare: %v", err)
	}
}

func TestCleanupKeepsTheActionCache(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	keep := filepath.Join(opts.Dir, "_work", "_actions", "checkout")
	drop := []string{
		filepath.Join(opts.Dir, "_work", "octarq-org"),
		filepath.Join(opts.Dir, "_work", "_temp"),
	}
	for _, p := range append([]string{keep}, drop...) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	l.Cleanup(context.Background())

	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("_actions was removed: %v", err)
	}
	for _, p := range drop {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived cleanup", p)
		}
	}
}

func TestRegisterPassesLabelsAndRedactsToken(t *testing.T) {
	exec := &fakeExec{}
	opts := testOptions(t, exec)
	l := New(opts)
	tok := model.RegistrationToken{Token: "ghs_supersecret"}
	if err := l.Register(context.Background(), tok, "runlet-box-lane-1-abcd1234"); err != nil {
		t.Fatal(err)
	}
	if len(exec.commands) != 1 {
		t.Fatalf("commands = %d", len(exec.commands))
	}
	cmd := exec.commands[0]
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--ephemeral", "--unattended", "--name runlet-box-lane-1-abcd1234",
		"--labels self-hosted,linux,x64", "--token ghs_supersecret"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(cmd.String(), "ghs_supersecret") {
		t.Fatalf("String() leaked the token: %s", cmd.String())
	}
	if !strings.Contains(cmd.String(), "<redacted>") {
		t.Fatalf("String() = %q, want a redaction marker", cmd.String())
	}
}

func TestRegisterSurfacesFailureWithoutToken(t *testing.T) {
	exec := &fakeExec{err: errors.New("config.sh exited 1")}
	l := New(testOptions(t, exec))
	tok := "token-that-must-not-be-logged"
	err := l.Register(context.Background(), model.RegistrationToken{Token: tok}, "runlet-box-lane-1-x")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatalf("error leaked the token: %v", err)
	}
}

// TestPrepareFollowsSymlinkedInstall: the installed runner keeps bin and
// externals as symlinks to bin.<version>. filepath.Walk does not descend into
// a symlinked root, so Prepare must resolve it.
func TestPrepareFollowsSymlinkedInstall(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	// Rebuild the source in the official layout, without a version file.
	if err := os.Remove(filepath.Join(opts.RunnerDir, "version")); err != nil {
		t.Fatal(err)
	}
	versioned := filepath.Join(opts.RunnerDir, "bin.2.338.0")
	if err := os.MkdirAll(versioned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(opts.RunnerDir, "bin", "Runner.Listener"), filepath.Join(versioned, "Runner.Listener")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(opts.RunnerDir, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versioned, filepath.Join(opts.RunnerDir, "bin")); err != nil {
		t.Fatal(err)
	}

	l := New(opts)
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(opts.Dir, "bin", "Runner.Listener")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("bin/Runner.Listener missing from the lane: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("lane copy is not a regular file: %v", info.Mode())
	}
}

// TestEnvKeepsTheUserBus: systemd-run --user finds the user manager only
// through XDG_RUNTIME_DIR.
func TestEnvKeepsTheUserBus(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	l := New(testOptions(t, &fakeExec{}))
	for _, kv := range l.env() {
		if kv == "XDG_RUNTIME_DIR=/run/user/1000" {
			return
		}
	}
	t.Fatal("env drops XDG_RUNTIME_DIR; systemd-run --user cannot connect to the bus")
}

// TestPrepareCopiesEveryTopLevelFile: run.sh sources run-helper.sh.template and
// safe_sleep.sh from its own directory. A copy that lists only config.sh and
// run.sh makes the runner exit 0 before it ever listens.
func TestPrepareCopiesEveryTopLevelFile(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.sh", "run.sh", "run-helper.sh.template", "safe_sleep.sh"} {
		if _, err := os.Stat(filepath.Join(opts.Dir, name)); err != nil {
			t.Fatalf("%s missing from the lane: %v", name, err)
		}
	}
	for _, name := range []string{".runner", ".runner_migrated", ".credentials", ".path", "actions-runner.tar.gz"} {
		if _, err := os.Stat(filepath.Join(opts.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s leaked from the source install into the lane", name)
		}
	}
}

// TestPrepareDropsThePreviousRegistration: config.sh refuses to run while
// .runner exists, and a killed supervisor always leaves one behind.
func TestPrepareDropsThePreviousRegistration(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	stale := map[string]string{".runner": "{}", ".runner_migrated": "{}", ".credentials": "{}", ".credentials_rsaparams": "key"}
	for name, body := range stale {
		if err := os.WriteFile(filepath.Join(opts.Dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	for name := range stale {
		if _, err := os.Stat(filepath.Join(opts.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived Prepare; the next config.sh fails with 'already configured'", name)
		}
	}
}

// TestUnitCommandsUseTheScopeSuffix: systemctl resolves a bare name to
// .service, so "is-active runlet-lane-0-abcd" always reports inactive and
// adoption silently skips every live scope.
func TestUnitCommandsUseTheScopeSuffix(t *testing.T) {
	exec := &fakeExec{}
	l := New(testOptions(t, exec))
	const unit = "runlet-lane-1-abcd1234"
	l.IsActive(context.Background(), unit)
	if err := l.Stop(context.Background(), unit); err != nil {
		t.Fatal(err)
	}
	if len(exec.commands) != 2 {
		t.Fatalf("commands = %d, want 2", len(exec.commands))
	}
	for _, c := range exec.commands {
		joined := strings.Join(c.Args, " ")
		if !strings.Contains(joined, unit+".scope") {
			t.Fatalf("%s does not address the scope unit", joined)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	l := New(testOptions(t, &fakeExec{}))
	st := l.Attempt()
	if err := l.SaveState(st); err != nil {
		t.Fatal(err)
	}
	got, err := l.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerName != st.RunnerName || got.Unit != st.Unit {
		t.Fatalf("got %+v, want %+v", got, st)
	}
	if err := l.ClearState(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.LoadState(); !os.IsNotExist(err) {
		t.Fatalf("state survived ClearState: %v", err)
	}
}

// TestCleanupLeavesASymlinkedWorkAlone: a job can replace _work with a symlink.
// RemoveAll on the entries would then delete files outside the lane.
func TestCleanupLeavesASymlinkedWorkAlone(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(opts.Dir, "_work")
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, work); err != nil {
		t.Fatal(err)
	}

	l.Cleanup(context.Background())

	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("cleanup followed a symlinked _work: %v", err)
	}
}

// TestCleanupLeavesASymlinkedDiagAlone: rotateDiag walks _diag, so a symlinked
// directory would let a job age out files it does not own.
func TestCleanupLeavesASymlinkedDiagAlone(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	old := filepath.Join(outside, "old.log")
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(opts.Dir, "_diag")); err != nil {
		t.Fatal(err)
	}

	l.Cleanup(context.Background())

	if _, err := os.Stat(old); err != nil {
		t.Fatalf("rotateDiag followed a symlinked _diag: %v", err)
	}
}

// TestPrepareReplacesASymlinkedWork: the runner writes _work, so the lane needs
// a real directory there. A symlink left by a job must be replaced, and its
// target must survive.
func TestPrepareReplacesASymlinkedWork(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(opts.Dir, "_work")); err != nil {
		t.Fatal(err)
	}

	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(filepath.Join(opts.Dir, "_work"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("_work is still %s", info.Mode())
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("the symlink target was deleted: %v", err)
	}
}

// TestPrepareRecopiesLaunchFiles: bin/externals are skipped once the version
// stamp matches, and launch scripts must not be. A job can rewrite config.sh;
// the next attempt has to put the real one back.
func TestPrepareRecopiesLaunchFiles(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(opts.Dir, "config.sh")
	if err := os.WriteFile(dst, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "#!/bin/sh\n" {
		t.Fatalf("config.sh was not restored: %q", raw)
	}
}

// TestCopyFileRefusesASymlinkedSource: the source install belongs to the
// operator. A symlink there must not turn into a readable copy in the lane.
func TestCopyFileRefusesASymlinkedSource(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.sh")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(link, filepath.Join(dir, "copy")); err == nil {
		t.Fatal("copyFile followed a symlinked source")
	}
}

// TestRunDoesNotHangOnAPipeHeldByAGrandchild: the runner is a grandchild of
// runlet (systemd-run -> run.sh) and inherits stdout. When the direct child
// exits, the pipe stays open. Without WaitDelay, Stop blocks until the job
// ends and systemd gives up and SIGKILLs the supervisor.
func TestRunDoesNotHangOnAPipeHeldByAGrandchild(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	done := make(chan error, 1)
	go func() {
		done <- OS{}.Run(context.Background(), Command{Path: "/bin/sh", Args: []string{"-c", "sleep 30 & exit 0"}})
	}()
	select {
	case <-done:
	case <-time.After(3 * waitDelay):
		t.Fatal("Run blocked on a pipe held open by a grandchild")
	}
}

// TestPrepareEmptiesTheLaneTempDirectory: an interrupted build leaves its
// temporary files behind, and the next attempt must start without them.
func TestPrepareEmptiesTheLaneTempDirectory(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	tmp := filepath.Join(opts.Dir, "tmp")
	if err := os.MkdirAll(filepath.Join(tmp, "go-build999"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp directory still holds %d entries after Prepare", len(entries))
	}
}

// TestPrepareReplacesASymlinkedTempDirectory: the lane is job-writable, so a
// job can point tmp at a directory outside the lane. Prepare must replace it
// instead of writing, or deleting, through it.
func TestPrepareReplacesASymlinkedTempDirectory(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(opts.Dir, "tmp")); err != nil {
		t.Fatal(err)
	}
	if err := l.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("Prepare deleted a file outside the lane: %v", err)
	}
	info, err := os.Lstat(filepath.Join(opts.Dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("tmp is still a symlink after Prepare")
	}
}

// TestCleanupEmptiesTheLaneTempDirectory: the job's last writes stay in TMPDIR
// when the runner exits; a lane that never cleans them grows without bound.
func TestCleanupEmptiesTheLaneTempDirectory(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	l := New(opts)
	tmp := filepath.Join(opts.Dir, "tmp")
	if err := os.MkdirAll(filepath.Join(tmp, "go-build888"), 0o700); err != nil {
		t.Fatal(err)
	}
	l.Cleanup(context.Background())
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp directory still holds %d entries after Cleanup", len(entries))
	}
}

func TestScopeAndEnvWithPoolEnv(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	opts.Env = map[string]string{
		"HTTPS_PROXY": "http://127.0.0.1:7890",
		"HTTP_PROXY":  "http://127.0.0.1:7890",
		"NO_PROXY":    "localhost,127.0.0.1",
		"B_KEY":       "b_val",
		"A_KEY":       "a_val",
	}
	l := New(opts)

	cmd := l.Scope("unit-test-env")

	joinedArgs := strings.Join(cmd.Args, " ")
	if !strings.Contains(joinedArgs, "--setenv=HTTPS_PROXY=http://127.0.0.1:7890") {
		t.Fatalf("Scope args missing HTTPS_PROXY: %v", cmd.Args)
	}
	wantHome := "--setenv=HOME=" + l.homeDir()
	wantTmp := "--setenv=TMPDIR=" + l.tmpDir()
	if !strings.Contains(joinedArgs, wantHome) {
		t.Fatalf("Scope args missing HOME: %v", cmd.Args)
	}
	if !strings.Contains(joinedArgs, wantTmp) {
		t.Fatalf("Scope args missing TMPDIR: %v", cmd.Args)
	}

	tmpIdx := -1
	workDirIdx := -1
	aKeyIdx := -1
	bKeyIdx := -1
	httpsProxyIdx := -1
	for i, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--setenv=TMPDIR=") {
			tmpIdx = i
		}
		if strings.HasPrefix(arg, "--working-directory=") {
			workDirIdx = i
		}
		if arg == "--setenv=A_KEY=a_val" {
			aKeyIdx = i
		}
		if arg == "--setenv=B_KEY=b_val" {
			bKeyIdx = i
		}
		if arg == "--setenv=HTTPS_PROXY=http://127.0.0.1:7890" {
			httpsProxyIdx = i
		}
	}
	if tmpIdx == -1 || workDirIdx == -1 || aKeyIdx == -1 || bKeyIdx == -1 || httpsProxyIdx == -1 {
		t.Fatalf("missing expected args in Scope: %v", cmd.Args)
	}
	if !(tmpIdx < aKeyIdx && aKeyIdx < bKeyIdx && bKeyIdx < httpsProxyIdx && httpsProxyIdx < workDirIdx) {
		t.Fatalf("Scope args order mismatch: tmp=%d a=%d b=%d https=%d workdir=%d",
			tmpIdx, aKeyIdx, bKeyIdx, httpsProxyIdx, workDirIdx)
	}

	env := l.env()
	envMap := make(map[string]string)
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			envMap[k] = v
		}
	}
	if got := envMap["HTTPS_PROXY"]; got != "http://127.0.0.1:7890" {
		t.Fatalf("env() HTTPS_PROXY = %q, want %q", got, "http://127.0.0.1:7890")
	}
	if got := envMap["A_KEY"]; got != "a_val" {
		t.Fatalf("env() A_KEY = %q, want %q", got, "a_val")
	}
	if got := envMap["HOME"]; got != l.homeDir() {
		t.Fatalf("env() HOME overwritten: %q, want %q", got, l.homeDir())
	}
	if got := envMap["LANG"]; got != "C.UTF-8" {
		t.Fatalf("env() LANG overwritten: %q, want C.UTF-8", got)
	}
}

func TestScopeAndEnvWithoutPoolEnv(t *testing.T) {
	opts := testOptions(t, &fakeExec{})
	opts.Env = nil
	l := New(opts)

	cmd := l.Scope("unit-test-no-env")
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "--setenv=CUSTOM_ENV_VAR=") {
			t.Fatalf("unexpected custom env in Scope args: %v", cmd.Args)
		}
	}

	env := l.env()
	for _, entry := range env {
		if strings.HasPrefix(entry, "CUSTOM_ENV_VAR=") {
			t.Fatalf("unexpected custom env in env(): %v", env)
		}
	}
}
