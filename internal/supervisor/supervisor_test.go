package supervisor

import (
	"bytes"
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
	// runs and jobs answer the watchdog: an in-progress run carries its
	// repository, and jobs are keyed by run id.
	runs        []map[string]any
	jobs        map[string][]map[string]any
	failRunners bool
	failJobs    bool
	// repos is what the org listing returns; orgCount is the organization's
	// own repository count, which a check compares against.
	repos    []string
	orgCount int
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
			if g.failRunners {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(g.runners), "runners": g.runners})
		case r.Method == http.MethodGet && r.URL.Path == "/orgs/acme":
			total := g.orgCount
			if total == 0 {
				total = len(stubRepos(g.repos))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"public_repos": total, "total_private_repos": 0})
		case r.Method == http.MethodGet && r.URL.Path == "/orgs/acme/repos":
			out := make([]map[string]any, 0)
			for _, name := range stubRepos(g.repos) {
				out = append(out, map[string]any{"full_name": name})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(g.runs), "workflow_runs": g.runs})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/jobs"):
			if g.failJobs {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			runID := parts[len(parts)-2]
			jobs := g.jobs[runID]
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(jobs), "jobs": jobs})
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

// staleFixture puts one live lane in the supervisor: a saved attempt, an
// adopted unit, and a busy registration on the stub.
func staleFixture(t *testing.T, exec *fakeExec, stub *ghStub) (*Supervisor, lane.State) {
	t.Helper()
	s := testSupervisor(t, exec, stub)
	st := s.lanes[0].Attempt()
	if err := s.lanes[0].SaveState(st); err != nil {
		t.Fatal(err)
	}
	s.limits[0] = st.Unit
	stub.runners = []map[string]any{{"id": 1, "name": st.RunnerName, "status": "online", "busy": true}}
	return s, st
}

// TestWatchdogStopsALaneGitHubHasNoJobFor: a cancelled job can leave a detached
// process tree behind. The runner reports busy forever, no in-progress job
// claims it, and the lane never takes new work.
func TestWatchdogStopsALaneGitHubHasNoJobFor(t *testing.T) {
	exec := &fakeExec{}
	s, st := staleFixture(t, exec, &ghStub{})
	now := time.Now()

	s.checkStale(context.Background(), now)
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog stopped a lane on the first miss")
	}
	s.checkStale(context.Background(), now.Add(time.Minute))
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog stopped a lane inside the window")
	}

	s.checkStale(context.Background(), now.Add(14*time.Minute))
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog stopped a lane inside the 15 minute window")
	}

	s.checkStale(context.Background(), now.Add(16*time.Minute))
	got := exec.find("stop " + st.Unit)
	if len(got) != 1 {
		t.Fatalf("stale lane stop calls = %d, want 1", len(got))
	}
}

// TestWatchdogLeavesALaneWithAnInProgressJob: the runner is busy because a job
// is running, which is exactly the normal case.
func TestWatchdogLeavesALaneWithAnInProgressJob(t *testing.T) {
	exec := &fakeExec{}
	stub := &ghStub{}
	s, st := staleFixture(t, exec, stub)
	stub.runs = []map[string]any{
		{"id": 5, "repository": map[string]any{"full_name": "acme/repo"}},
	}
	stub.jobs = map[string][]map[string]any{
		"5": {{"id": 1, "runner_name": st.RunnerName, "status": "in_progress"}},
	}

	now := time.Now()
	s.checkStale(context.Background(), now)
	s.checkStale(context.Background(), now.Add(20*time.Minute))
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog stopped a lane that is running a job")
	}
}

// TestWatchdogIgnoresAnIdleLane: an idle runner is not busy, and stopping it
// would only cost a registration.
func TestWatchdogIgnoresAnIdleLane(t *testing.T) {
	exec := &fakeExec{}
	stub := &ghStub{}
	s, st := staleFixture(t, exec, stub)
	stub.runners[0]["busy"] = false

	now := time.Now()
	s.checkStale(context.Background(), now)
	s.checkStale(context.Background(), now.Add(20*time.Minute))
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog stopped an idle lane")
	}
}

// TestWatchdogDoesNotStopOnAnAPIError: without a complete answer the watchdog
// must do nothing. A wrong stop fails a running job.
func TestWatchdogDoesNotStopOnAnAPIError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stub  *ghStub
		setup func(*ghStub)
	}{
		{name: "runners unavailable", stub: &ghStub{failRunners: true}},
		{name: "jobs unavailable", stub: &ghStub{failJobs: true}, setup: func(g *ghStub) {
			g.runs = []map[string]any{{"id": 5, "repository": map[string]any{"full_name": "acme/repo"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExec{}
			s, st := staleFixture(t, exec, tc.stub)
			if tc.setup != nil {
				tc.setup(tc.stub)
			}

			now := time.Now()
			s.checkStale(context.Background(), now)
			s.checkStale(context.Background(), now.Add(20*time.Minute))
			if got := exec.find("stop " + st.Unit); len(got) != 0 {
				t.Fatalf("the watchdog acted on a failed API call")
			}
		})
	}
}

// TestWatchdogForgetsAMissWhenTheJobShowsUp: the API can lag a start. A lane
// that is seen with a job again starts its count from zero.
func TestWatchdogForgetsAMissWhenTheJobShowsUp(t *testing.T) {
	exec := &fakeExec{}
	stub := &ghStub{}
	s, st := staleFixture(t, exec, stub)
	now := time.Now()

	s.checkStale(context.Background(), now)

	stub.mu.Lock()
	stub.runs = []map[string]any{{"id": 5, "repository": map[string]any{"full_name": "acme/repo"}}}
	stub.jobs = map[string][]map[string]any{"5": {{"id": 1, "runner_name": st.RunnerName, "status": "in_progress"}}}
	stub.mu.Unlock()

	s.checkStale(context.Background(), now.Add(9*time.Minute))

	stub.mu.Lock()
	stub.runs = nil
	stub.mu.Unlock()
	// Nine minutes of the threshold were spent with a job; the miss starts
	// again here, so this check must not stop the lane.
	s.checkStale(context.Background(), now.Add(10*time.Minute))
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog kept counting a miss that had cleared")
	}
}

// TestWatchdogStopsNothingWhenTheRepoListIsIncomplete: a repository the check
// cannot see looks exactly like a lane GitHub forgot. Acting on that would kill
// a running job.
func TestWatchdogStopsNothingWhenTheRepoListIsIncomplete(t *testing.T) {
	exec := &fakeExec{}
	stub := &ghStub{repos: []string{"acme/repo"}, orgCount: 9}
	s, st := staleFixture(t, exec, stub)

	now := time.Now()
	s.checkStale(context.Background(), now)
	s.checkStale(context.Background(), now.Add(20*time.Minute))
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog acted on an incomplete repository list")
	}
}

// TestWatchdogStopsALaneWhoseRegistrationIsGone: an ephemeral runner deletes
// its registration when the job ends, and a lane can then sit on a detached
// process tree with no registration at all. GitHub has nothing to report busy,
// so the runner list alone can never show it.
func TestWatchdogStopsALaneWhoseRegistrationIsGone(t *testing.T) {
	exec := &fakeExec{}
	stub := &ghStub{}
	s, st := staleFixture(t, exec, stub)
	stub.mu.Lock()
	stub.runners = nil
	stub.mu.Unlock()

	now := time.Now()
	s.checkStale(context.Background(), now)
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog stopped a lane on the first miss")
	}
	s.checkStale(context.Background(), now.Add(16*time.Minute))
	if got := exec.find("stop " + st.Unit); len(got) != 1 {
		t.Fatalf("stale lane stop calls = %d, want 1", len(got))
	}
}

// TestWatchdogLeavesALaneWhoseJobOutlivedItsRegistration: the registration can
// be deleted while GitHub still has a job in progress for that runner. Stopping
// the lane there fails the job.
func TestWatchdogLeavesALaneWhoseJobOutlivedItsRegistration(t *testing.T) {
	exec := &fakeExec{}
	stub := &ghStub{}
	s, st := staleFixture(t, exec, stub)
	stub.mu.Lock()
	stub.runners = nil
	stub.runs = []map[string]any{{"id": 5, "repository": map[string]any{"full_name": "acme/repo"}}}
	stub.jobs = map[string][]map[string]any{
		"5": {{"id": 1, "runner_name": st.RunnerName, "status": "in_progress"}},
	}
	stub.mu.Unlock()

	now := time.Now()
	s.checkStale(context.Background(), now)
	s.checkStale(context.Background(), now.Add(20*time.Minute))
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog stopped a lane whose job is in progress")
	}
}

// TestWatchdogReadsEveryRepository: the job can be in any repository of the
// organization, not only in the ones the policy allowlists.
func TestWatchdogReadsEveryRepository(t *testing.T) {
	exec := &fakeExec{}
	stub := &ghStub{repos: []string{"acme/one", "acme/two"}}
	s, st := staleFixture(t, exec, stub)
	stub.runs = []map[string]any{{"id": 9}}
	stub.jobs = map[string][]map[string]any{
		"9": {{"id": 1, "runner_name": st.RunnerName, "status": "in_progress"}},
	}
	// The stub answers the same run for every repository path, which is enough
	// to show that a job in a second repository is seen.
	now := time.Now()
	s.checkStale(context.Background(), now)
	s.checkStale(context.Background(), now.Add(20*time.Minute))
	if got := exec.find("stop " + st.Unit); len(got) != 0 {
		t.Fatalf("the watchdog ignored a job in another repository")
	}
}

// stubRepos is what the org listing returns when a test does not name any.
func stubRepos(repos []string) []string {
	if repos == nil {
		return []string{"acme/repo"}
	}
	return repos
}

// TestNewWarnsWhenMemoryHighIsBelowMemory: on the mean host a lane that reclaims
// its own page cache re-reads every build input from disk. The warning is the
// only signal an operator gets before the host's disk saturates.
func TestNewWarnsWhenMemoryHighIsBelowMemory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		high    config.Bytes
		wantLog bool
	}{
		{name: "high below max", high: config.Bytes(1600 << 20), wantLog: true},
		{name: "high equals max", high: config.Bytes(2 << 30), wantLog: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			srv := httptest.NewServer((&ghStub{}).handler())
			t.Cleanup(srv.Close)
			dir := t.TempDir()
			cfg := &config.Config{Pool: config.PoolConfig{
				StateDir:     filepath.Join(dir, "state"),
				WorkDir:      filepath.Join(dir, "work"),
				RunnerDir:    fakeRunnerDir(t),
				RunnerURL:    "https://github.com/acme",
				MemoryBudget: config.Bytes(4 << 30),
				Lanes: []config.LaneConfig{{
					Name:       "lane-0",
					Labels:     []string{"self-hosted"},
					Memory:     config.Bytes(2 << 30),
					MemoryHigh: tc.high,
					CPUQuota:   "200%",
				}},
			}}
			if _, err := New(Options{
				Config: cfg,
				Client: ghapi.New("tok", ghapi.WithBaseURL(srv.URL)),
				Exec:   &fakeExec{},
				Log:    slog.New(slog.NewTextHandler(&buf, nil)),
			}); err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(buf.String(), "memory_high is below memory")
			if got != tc.wantLog {
				t.Fatalf("warning logged = %v, want %v; log: %s", got, tc.wantLog, buf.String())
			}
		})
	}
}

// TestScratchCheckSweepsOncePerDayAndReportsUsage: the sweep is the repair, the
// gauge is the signal. The sweep must not run on every check: it walks the
// shared temp directory.
func TestScratchCheckSweepsOncePerDayAndReportsUsage(t *testing.T) {
	s := testSupervisor(t, &fakeExec{}, &ghStub{})
	dir := t.TempDir()
	now := time.Now()
	sc := config.ScratchConfig{Path: dir}

	mkOld := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-13 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}

	stale := mkOld("go-build1")
	last := s.checkScratch(sc, time.Time{}, now)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the first check did not sweep the stale directory: %v", err)
	}
	found := false
	for k := range s.opts.Metrics.Snapshot() {
		if strings.HasPrefix(k, "runlet_scratch_used_ratio") {
			found = true
		}
	}
	if !found {
		t.Fatal("the usage gauge was not reported")
	}

	// Inside the interval the sweep must not run again.
	again := mkOld("go-build2")
	if got := s.checkScratch(sc, last, now.Add(time.Hour)); !got.Equal(last) {
		t.Fatalf("the sweep ran inside its interval")
	}
	if _, err := os.Stat(again); err != nil {
		t.Fatalf("a second sweep ran inside the interval: %v", err)
	}

	// After the interval it runs again.
	s.checkScratch(sc, last, now.Add(25*time.Hour))
	if _, err := os.Stat(again); !os.IsNotExist(err) {
		t.Fatalf("the sweep did not run after its interval")
	}
}

// TestScratchWarnsOncePerCrossing: a warning every ten minutes buries the line
// that matters.
func TestScratchWarnsOncePerCrossing(t *testing.T) {
	var buf bytes.Buffer
	s := testSupervisor(t, &fakeExec{}, &ghStub{})
	s.opts.Log = slog.New(slog.NewTextHandler(&buf, nil))
	dir := t.TempDir()
	one := 1
	sc := config.ScratchConfig{Path: dir, WarnPercent: &one}

	now := time.Now()
	s.checkScratch(sc, now, now)
	s.checkScratch(sc, now, now.Add(scratchCheck))
	if got := strings.Count(buf.String(), "scratch is nearly full"); got != 1 {
		t.Fatalf("warnings = %d, want 1; log: %s", got, buf.String())
	}
}
