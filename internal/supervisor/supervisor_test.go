package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jungley8/runlet/internal/config"
	"github.com/Jungley8/runlet/internal/ghapi"
	"github.com/Jungley8/runlet/internal/lane"
	"github.com/Jungley8/runlet/internal/metrics"
)

type fakeExec struct {
	mu       sync.Mutex
	commands []lane.Command
	units    string
	active   bool
	// started maps a unit name to its ActiveEnterTimestampMonotonic, the value
	// adoption uses to tell a stale scope from the live one.
	started map[string]string
	// block makes the lane scope behave like a real one: systemd-run returns
	// only when the context is cancelled, not when the scope's processes exit.
	block bool
}

func (f *fakeExec) Run(ctx context.Context, c lane.Command) error {
	f.mu.Lock()
	f.commands = append(f.commands, c)
	block := f.block
	f.mu.Unlock()
	if block && strings.Contains(c.Path, "systemd-run") {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeExec) Output(_ context.Context, c lane.Command) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, c)
	joined := strings.Join(c.Args, " ")
	switch {
	case strings.Contains(joined, "ActiveEnterTimestampMonotonic"):
		for unit, ts := range f.started {
			if strings.Contains(joined, unit) {
				return ts, nil
			}
		}
		return "", nil
	case strings.Contains(joined, "list-units"):
		return f.units, nil
	case strings.Contains(joined, "is-active"):
		if f.active {
			return "", nil
		}
		return "", errors.New("inactive")
	}
	return "", nil
}

func (f *fakeExec) find(substr string) []lane.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []lane.Command
	for _, c := range f.commands {
		if strings.Contains(c.String(), substr) {
			out = append(out, c)
		}
	}
	return out
}

func fakeRunnerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"config.sh", "run.sh", "version", "bin/Runner.Listener", "externals/node"} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "#!/bin/sh\n"
		if name == "version" {
			body = "2.338.0\n"
		}
		if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// ghStub answers the two runner endpoints the supervisor uses.
type ghStub struct {
	mu      sync.Mutex
	tokens  int
	deleted []int64
	runners []map[string]any
}

func (g *ghStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/registration-token"):
			g.tokens++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "reg-token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/runners"):
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(g.runners), "runners": g.runners})
		case r.Method == http.MethodDelete:
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			id, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			g.deleted = append(g.deleted, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func testSupervisor(t *testing.T, exec lane.Execer, stub *ghStub) *Supervisor {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		Policy: config.PolicyConfig{},
		Pool: config.PoolConfig{
			StateDir:     filepath.Join(dir, "state"),
			WorkDir:      filepath.Join(dir, "work"),
			RunnerDir:    fakeRunnerDir(t),
			RunnerURL:    "https://github.com/acme",
			MemoryBudget: config.Bytes(3 << 30),
			Lanes: []config.LaneConfig{{
				Name:     "lane-0",
				Labels:   []string{"self-hosted", "linux"},
				Memory:   config.Bytes(1 << 30),
				CPUQuota: "200%",
			}},
		},
	}
	s, err := New(Options{
		Config:  cfg,
		Client:  ghapi.New("tok", ghapi.WithBaseURL(srv.URL)),
		Exec:    exec,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAttemptRegistersAndCleansUp: one lane life must mint a token, configure
// the runner, run it in a scope, then deregister and drop the state file.
func TestAttemptRegistersAndCleansUp(t *testing.T) {
	exec := &fakeExec{}
	stub := &ghStub{runners: []map[string]any{{"id": 77, "name": "x", "status": "online"}}}
	s := testSupervisor(t, exec, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if stub.tokens != 1 {
		t.Fatalf("registration tokens minted = %d, want 1", stub.tokens)
	}
	register := exec.find("config.sh")
	if len(register) != 1 {
		t.Fatalf("config.sh calls = %d, want 1", len(register))
	}
	joined := strings.Join(register[0].Args, " ")
	for _, want := range []string{"--ephemeral", "--labels self-hosted,linux", "--token reg-token"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("config.sh missing %q: %s", want, joined)
		}
	}
	if len(exec.find("systemd-run")) != 1 {
		t.Fatalf("systemd-run calls = %d", len(exec.find("systemd-run")))
	}
	if _, err := os.Stat(filepath.Join(s.opts.Config.Pool.StateDir, "lanes", "lane-0", "lane.json")); !os.IsNotExist(err) {
		t.Fatalf("lane state was not cleared: %v", err)
	}
}

// TestAdoptDoesNotStartASecondRunner: after a supervisor restart, a live scope
// must be adopted, not duplicated.
func TestAdoptDoesNotStartASecondRunner(t *testing.T) {
	exec := &fakeExec{
		units:  "runlet-lane-0-abcd1234.scope loaded active running runlet-lane-0-abcd1234\n",
		active: true,
	}
	stub := &ghStub{}
	s := testSupervisor(t, exec, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := exec.find("config.sh"); len(got) != 0 {
		t.Fatalf("adoption started a second runner: %d config.sh calls", len(got))
	}
	if stub.tokens != 0 {
		t.Fatalf("adoption minted a registration token")
	}
}

// TestDrainStartsNothing: the drain flag stops new attempts without a restart.
func TestDrainStartsNothing(t *testing.T) {
	exec := &fakeExec{}
	stub := &ghStub{}
	s := testSupervisor(t, exec, stub)

	if err := os.MkdirAll(s.opts.Config.Pool.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.drain, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := exec.find("config.sh"); len(got) != 0 {
		t.Fatalf("drain started a lane: %d config.sh calls", len(got))
	}
}

func TestNewDefaultsTheExecer(t *testing.T) {
	srv := httptest.NewServer((&ghStub{}).handler())
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{Pool: config.PoolConfig{
		StateDir:  filepath.Join(dir, "state"),
		WorkDir:   filepath.Join(dir, "work"),
		RunnerDir: fakeRunnerDir(t),
		RunnerURL: "https://github.com/acme",
		Lanes:     []config.LaneConfig{{Name: "lane-0", Labels: []string{"self-hosted"}, Memory: config.Bytes(1 << 30)}},
	}}
	s, err := New(Options{Config: cfg, Client: ghapi.New("tok", ghapi.WithBaseURL(srv.URL))})
	if err != nil {
		t.Fatal(err)
	}
	if s.opts.Exec == nil {
		t.Fatal("Exec is nil; a nil execer panics on the first lane attempt")
	}
}

// TestAdoptStopsDuplicateScopes: two live scopes for one slot mean an earlier
// supervisor left one behind. Keep the newest, stop the rest, start nothing new.
func TestAdoptStopsDuplicateScopes(t *testing.T) {
	exec := &fakeExec{
		units: "runlet-lane-0-aaaa1111.scope loaded active running x\n" +
			"runlet-lane-0-bbbb2222.scope loaded active running x\n",
		active: true,
		started: map[string]string{
			"runlet-lane-0-aaaa1111.scope": "1000",
			"runlet-lane-0-bbbb2222.scope": "2000",
		},
	}
	s := testSupervisor(t, exec, &ghStub{})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// list-units order is not start order; bbbb2222 started later and must win.
	if len(exec.find("stop runlet-lane-0-aaaa1111.scope")) != 1 {
		t.Fatalf("the older scope was not stopped")
	}
	if len(exec.find("stop runlet-lane-0-bbbb2222.scope")) != 0 {
		t.Fatalf("the adopted scope was stopped")
	}
	if got := exec.find("config.sh"); len(got) != 0 {
		t.Fatalf("adoption started %d runners", len(got))
	}
}

// TestSweepDeletesOfflineRegistrationsItDoesNotOwn: a killed lane leaves a
// registration that can still receive a job.
func TestSweepDeletesOfflineRegistrationsItDoesNotOwn(t *testing.T) {
	stub := &ghStub{runners: []map[string]any{
		{"id": 1, "name": "dead", "status": "offline"},
		{"id": 2, "name": "live", "status": "online"},
		{"id": 3, "name": "someone-else", "status": "offline"},
		{"id": 4, "name": "lookalike", "status": "offline"},
	}}
	s := testSupervisor(t, &fakeExec{}, stub)
	stub.runners[0]["name"] = "runlet-" + s.host + "-lane-0-deadbeef"
	stub.runners[1]["name"] = "runlet-" + s.host + "-lane-1-cafebabe"
	// Right prefix, but the suffix is not the 8 hex chars this instance mints.
	stub.runners[3]["name"] = "runlet-" + s.host + "-lane-2-notours"

	s.sweepStale(context.Background())

	if len(stub.deleted) != 1 || stub.deleted[0] != 1 {
		t.Fatalf("deleted %v, want [1]", stub.deleted)
	}
}

func TestUnitIndex(t *testing.T) {
	cases := map[string]struct {
		index int
		ok    bool
	}{
		"runlet-lane-0-abcd1234":  {0, true},
		"runlet-lane-12-ffffffff": {12, true},
		"other-scope":             {0, false},
		"runlet-lane-x-abcd":      {0, false},
		"runlet-lane-1":           {0, false},
	}
	for unit, want := range cases {
		got, ok := unitIndex(unit)
		if ok != want.ok || (ok && got != want.index) {
			t.Fatalf("%q = (%d,%v), want (%d,%v)", unit, got, ok, want.index, want.ok)
		}
	}
}

func TestLockIsExclusive(t *testing.T) {
	s := testSupervisor(t, &fakeExec{}, &ghStub{})
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if _, err := s.lock(); err == nil {
		t.Fatal("a second supervisor took the lock")
	}
}

// TestShutdownLeavesTheScopeForAdoption: a supervisor stop must not kill a job.
// The scope keeps running, so the state file has to stay on disk: the next
// process adopts the unit and uses that name to deregister it.
func TestShutdownLeavesTheScopeForAdoption(t *testing.T) {
	exec := &fakeExec{block: true}
	s := testSupervisor(t, exec, &ghStub{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	statePath := filepath.Join(s.opts.Config.Pool.StateDir, "lanes", "lane-0", "lane.json")
	// Cancel once the attempt is really inside systemd-run, so the test checks
	// the shutdown path and not a context that expired during setup.
	go func() {
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(statePath); err == nil && len(exec.find("systemd-run")) > 0 {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := exec.find("stop runlet-"); len(got) != 0 {
		t.Fatalf("shutdown stopped the lane scope: %s", got[0].String())
	}
	st, err := s.lanes[0].LoadState()
	if err != nil {
		t.Fatalf("state file was cleared on shutdown: %v\ncommands: %v", err, debugCommands(exec))
	}
	if st.RunnerName == "" || st.Unit == "" {
		t.Fatalf("state = %+v", st)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("%s: %v\ncommands: %v", statePath, err, debugCommands(exec))
	}
}

func debugCommands(f *fakeExec) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.commands {
		out = append(out, c.String())
	}
	return out
}

// TestAttemptClearsStateWhenTheScopeEnds: the same attempt on a live supervisor
// cleans up after itself, so the state file must not survive.
func TestAttemptClearsStateWhenTheScopeEnds(t *testing.T) {
	s := testSupervisor(t, &fakeExec{}, &ghStub{})
	if err := s.attempt(context.Background(), s.lanes[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.lanes[0].LoadState(); !os.IsNotExist(err) {
		t.Fatalf("state survived a finished attempt: %v", err)
	}
}
