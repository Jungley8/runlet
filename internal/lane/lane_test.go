package lane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
