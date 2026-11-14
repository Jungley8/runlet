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
}

func (f *fakeExec) Run(_ context.Context, c lane.Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, c)
	return nil
}

func (f *fakeExec) Output(_ context.Context, c lane.Command) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, c)
	joined := strings.Join(c.Args, " ")
	switch {
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
