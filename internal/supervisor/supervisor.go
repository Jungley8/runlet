// Package supervisor runs the configured lanes: it starts each runner under a
// systemd scope, adopts scopes that survived a restart, and cleans up after
// every exit path.
package supervisor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jungley8/runlet/internal/config"
	"github.com/Jungley8/runlet/internal/ghapi"
	"github.com/Jungley8/runlet/internal/lane"
	"github.com/Jungley8/runlet/internal/metrics"
	"github.com/Jungley8/runlet/internal/model"
	"github.com/Jungley8/runlet/internal/policy"
	"github.com/Jungley8/runlet/internal/scratch"
)

const maxBackoff = 5 * time.Minute

type Options struct {
	Config  *config.Config
	Client  *ghapi.Client // must be able to mint registration tokens
	Exec    lane.Execer
	Log     *slog.Logger
	Metrics *metrics.Metrics
}

type Supervisor struct {
	opts   Options
	scope  config.Scope
	host   string
	lanes  []*lane.Lane
	drain  string
	mu     sync.Mutex
	limits map[int]string // lane index -> adopted unit
	// miss remembers, per lane, when the watchdog first saw GitHub report the
	// runner busy with no job behind it.
	miss       map[int]time.Time
	staleAfter time.Duration
	watchEvery time.Duration
	// scratchWarned remembers a warning this process already printed, so the
	// log holds one line per crossing instead of one per check. The scratch
	// goroutine is its only writer.
	scratchWarned bool
}

func New(opts Options) (*Supervisor, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.New()
	}
	if opts.Exec == nil {
		opts.Exec = lane.OS{}
	}
	scope, err := config.ParseScope(opts.Config.Pool.RunnerURL)
	if err != nil {
		return nil, err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "host"
	}
	host = strings.Split(host, ".")[0]

	pool := opts.Config.Pool
	s := &Supervisor{
		opts:   opts,
		scope:  scope,
		host:   host,
		drain:  filepath.Join(pool.StateDir, "DRAIN"),
		limits: make(map[int]string),
		miss:   make(map[int]time.Time),
	}
	s.staleAfter = pool.StaleAfter()
	s.watchEvery = 2 * time.Minute
	if s.staleAfter > 0 && s.staleAfter/2 < s.watchEvery {
		s.watchEvery = s.staleAfter / 2
	}
	if s.watchEvery < 15*time.Second {
		s.watchEvery = 15 * time.Second
	}
	for i, lc := range pool.Lanes {
		if lc.MemoryHigh > 0 && lc.MemoryHigh < lc.Memory {
			// Measured on the macmini: at memory_high below memory the lane
			// reclaims its own page cache, and a Go compile then re-reads the
			// same sources from disk until the SSD is the bottleneck.
			opts.Log.Warn("memory_high is below memory: the lane will reclaim its page cache and re-read build inputs from disk",
				"lane", lc.Name, "memory_high", lc.MemoryHigh.Int64(), "memory", lc.Memory.Int64())
		}
		s.lanes = append(s.lanes, lane.New(lane.Options{
			Index:      i,
			Name:       lc.Name,
			Labels:     lc.Labels,
			Memory:     lc.Memory.Int64(),
			MemoryHigh: lc.MemoryHigh.Int64(),
			CPUQuota:   lc.CPUQuota,
			RunnerDir:  pool.RunnerDir,
			RunnerURL:  scope.URL(),
			Dir:        filepath.Join(pool.WorkDir, lc.Name),
			StateDir:   filepath.Join(pool.StateDir, "lanes", lc.Name),
			CacheDir:   filepath.Join(pool.WorkDir, "cache"),
			Host:       host,
			Exec:       opts.Exec,
			Log:        opts.Log,
		}))
	}
	if len(s.lanes) == 0 {
		return nil, errors.New("no lanes configured")
	}
	return s, nil
}

// Run starts every lane and blocks until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	s.adopt(ctx)
	s.sweepStale(ctx)

	var wg sync.WaitGroup
	for _, l := range s.lanes {
		wg.Add(1)
		go func(l *lane.Lane) {
			defer wg.Done()
			s.loop(ctx, l)
		}(l)
	}
	if s.staleAfter > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.watch(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.watchScratch(ctx)
	}()
	<-ctx.Done()
	wg.Wait()
	return nil
}

// scratchCheck is how often the temp directory is measured. The gauge is cheap;
// the sweep runs on its own, much longer, interval.
const scratchCheck = 10 * time.Minute

// watchScratch reports how full the shared temp directory is, and removes stale
// build directories from it. A host whose /tmp is a tmpfs loses memory to every
// interrupted build, and a full tmpfs fails every lane at once: the failure
// does not look like a build failure, it looks like a mysterious ENOSPC in
// whichever step happens to write a large file.
func (s *Supervisor) watchScratch(ctx context.Context) {
	t := time.NewTicker(scratchCheck)
	defer t.Stop()
	var last time.Time // zero: sweep on the first check
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			last = s.checkScratch(s.opts.Config.Pool.Scratch, last, now)
		}
	}
}

// checkScratch runs one measurement and, when due, one sweep. It returns the
// time of the last sweep.
func (s *Supervisor) checkScratch(sc config.ScratchConfig, last, now time.Time) time.Time {
	path := sc.PathValue()
	if every := sc.EveryFor(); every > 0 && (last.IsZero() || now.Sub(last) >= every) {
		removed, err := scratch.Sweep(path, sc.PatternValues(), sc.StaleFor(), now)
		switch {
		case err != nil:
			s.opts.Log.Warn("scratch sweep skipped", "path", path, "err", err)
		case removed > 0:
			s.opts.Log.Info("removed stale build directories from scratch", "path", path, "dirs", removed)
			s.opts.Metrics.Add("runlet_scratch_removed_total", map[string]string{"path": path}, float64(removed))
		}
		// A failed sweep waits for the next interval: a retry loop would walk
		// the same directory every ten minutes.
		last = now
	}

	u, err := scratch.UsageOf(path)
	if err != nil {
		s.opts.Log.Warn("scratch usage unavailable", "path", path, "err", err)
		return last
	}
	s.opts.Metrics.Set("runlet_scratch_used_ratio", map[string]string{"path": path}, u.Ratio())
	if u.Ratio() < sc.WarnRatio() {
		s.scratchWarned = false
		return last
	}
	if !s.scratchWarned {
		s.scratchWarned = true
		s.opts.Log.Warn("scratch is nearly full: the next build that writes a large temporary file will fail",
			"path", path, "used", u.Used, "total", u.Total, "percent", int(u.Ratio()*100))
	}
	return last
}

func (s *Supervisor) watch(ctx context.Context) {
	t := time.NewTicker(s.watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkStale(ctx, time.Now())
		}
	}
}

// adopt finds lane scopes that are still running. A supervisor restart must not
// start a second runner for the same slot: two runners with different names
// both register, and GitHub hands work to both. Extra scopes for one slot mean
// an earlier supervisor left them behind, so keep the newest and stop the rest.
func (s *Supervisor) adopt(ctx context.Context) {
	units, err := s.lanes[0].ListUnits(ctx)
	if err != nil {
		s.opts.Log.Warn("lane adoption skipped: cannot list units", "err", err)
		return
	}
	found := make(map[int][]string)
	for _, u := range units {
		idx, ok := unitIndex(u)
		if !ok || idx < 0 || idx >= len(s.lanes) {
			continue
		}
		if !s.lanes[idx].IsActive(ctx, u) {
			continue
		}
		found[idx] = append(found[idx], u)
	}
	for idx, list := range found {
		// Keep the one that started last. list-units order is not the start
		// order, and stopping the scope that is running a job fails that run.
		keep := list[0]
		kept := s.lanes[idx].StartedAt(ctx, keep)
		for _, u := range list[1:] {
			if t := s.lanes[idx].StartedAt(ctx, u); t > kept {
				keep, kept = u, t
			}
		}
		for _, u := range list {
			if u == keep {
				continue
			}
			s.opts.Log.Warn("stopping a duplicate lane scope", "lane", s.lanes[idx].Name(), "unit", u)
			_ = s.lanes[idx].Stop(ctx, u)
		}
		s.mu.Lock()
		s.limits[idx] = keep
		s.mu.Unlock()
		s.opts.Log.Info("adopted a running lane", "lane", s.lanes[idx].Name(), "unit", keep)
	}
}

// sweepStale deletes registrations that no live scope owns. A killed or
// duplicated lane leaves its registration until GitHub times it out, and every
// one of those can still receive a job.
func (s *Supervisor) sweepStale(ctx context.Context) {
	keep := make(map[string]bool)
	for _, l := range s.lanes {
		if st, err := l.LoadState(); err == nil && s.ownsRunner(st.RunnerName) {
			keep[st.RunnerName] = true
		}
	}
	runners, err := s.opts.Client.ListRunners(ctx, s.scope.APIPath())
	if err != nil {
		s.opts.Log.Warn("stale registration sweep skipped", "err", err)
		return
	}
	for _, r := range runners {
		if !s.ownsRunner(r.Name) || r.Status != "offline" || keep[r.Name] {
			continue
		}
		if err := s.opts.Client.DeleteRunner(ctx, s.scope.APIPath(), r.ID); err != nil {
			s.opts.Log.Warn("stale registration sweep failed", "runner", r.Name, "err", err)
			continue
		}
		s.opts.Log.Info("deleted a stale registration", "runner", r.Name, "id", r.ID)
	}
}

// ownsRunner reports whether a name is one this instance could have minted:
// runlet-<host>-lane-<index>-<8 hex>. A name read from a file that a job can
// write must pass this before it reaches the delete API.
func (s *Supervisor) ownsRunner(name string) bool {
	prefix := "runlet-" + s.host + "-lane-"
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	head, tail, ok := strings.Cut(rest, "-")
	if !ok {
		return false
	}
	if _, err := strconv.Atoi(head); err != nil {
		return false
	}
	if len(tail) != 8 {
		return false
	}
	_, err := hex.DecodeString(tail)
	return err == nil
}

func unitIndex(unit string) (int, bool) {
	rest, ok := strings.CutPrefix(unit, "runlet-lane-")
	if !ok {
		return 0, false
	}
	head, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(head)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (s *Supervisor) loop(ctx context.Context, l *lane.Lane) {
	backoff := time.Second
	for ctx.Err() == nil {
		if unit, ok := s.adopted(l.Index()); ok {
			s.waitForUnit(ctx, l, unit)
			continue
		}
		if s.draining() {
			s.opts.Log.Info("draining: not starting a new lane attempt", "lane", l.Name())
			if !sleep(ctx, 5*time.Second) {
				return
			}
			continue
		}

		err := s.attempt(ctx, l)
		s.opts.Metrics.Set("runlet_lane_up", map[string]string{"lane": l.Name()}, 0)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.opts.Metrics.Add("runlet_lane_errors_total", map[string]string{"lane": l.Name()}, 1)
			s.opts.Log.Warn("lane attempt failed", "lane", l.Name(), "err", err, "retry_in", backoff)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		if !sleep(ctx, time.Second) {
			return
		}
	}
}

// waitForUnit lets an adopted runner finish its job, then cleans up after it.
func (s *Supervisor) waitForUnit(ctx context.Context, l *lane.Lane, unit string) {
	for ctx.Err() == nil && l.IsActive(ctx, unit) {
		if !sleep(ctx, 5*time.Second) {
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = l.Stop(stopCtx, unit)
	l.Cleanup(context.Background())
	s.mu.Lock()
	delete(s.limits, l.Index())
	s.mu.Unlock()
	if st, err := l.LoadState(); err == nil {
		// The state file is job-writable. Only deregister the runner this very
		// attempt registered, or a crafted file turns the supervisor into a
		// confused deputy against the runner API.
		if st.Unit == unit && s.ownsRunner(st.RunnerName) {
			s.deregister(stopCtx, st.RunnerName)
		} else {
			s.opts.Log.Warn("ignoring a state file that does not match the adopted scope", "lane", l.Name(), "unit", unit, "state_unit", st.Unit, "runner", st.RunnerName)
		}
		_ = l.ClearState()
	}
}

func (s *Supervisor) adopted(index int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unit, ok := s.limits[index]
	return unit, ok
}

// attempt is one full life of a lane: prepare, register, run, clean up.
func (s *Supervisor) attempt(ctx context.Context, l *lane.Lane) error {
	if err := l.Prepare(); err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	st := l.Attempt()
	if err := l.SaveState(st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	// On shutdown the state file stays: the next process adopts the scope and
	// needs the runner name to deregister it. Every other exit clears it.
	keepState := false
	defer func() {
		if !keepState {
			_ = l.ClearState()
		}
	}()

	tok, err := s.opts.Client.CreateRegistrationToken(ctx, s.scope.APIPath())
	if err != nil {
		return fmt.Errorf("registration token: %w", err)
	}
	if !tok.ExpiresAt.IsZero() && time.Until(tok.ExpiresAt) < time.Minute {
		return errors.New("registration token expires within a minute")
	}

	if err := l.Register(ctx, tok, st.RunnerName); err != nil {
		return err
	}
	tok = model.RegistrationToken{}

	s.opts.Log.Info("lane registered", "lane", l.Name(), "runner", st.RunnerName, "unit", st.Unit)
	s.opts.Metrics.Add("runlet_lane_attempts_total", map[string]string{"lane": l.Name()}, 1)
	s.opts.Metrics.Set("runlet_lane_up", map[string]string{"lane": l.Name()}, 1)

	// Cancelling ctx kills systemd-run; the scope's processes survive that, so
	// the stop below runs on its own context.
	runErr := s.opts.Exec.Run(ctx, l.Scope(st.Unit))
	if ctx.Err() != nil {
		// Shutting down. Adopt, do not kill: the scope and its job outlive this
		// process, and the next supervisor picks the runner up by unit name.
		keepState = true
		return runErr
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = l.Stop(stopCtx, st.Unit)
	l.Cleanup(context.Background())
	s.deregister(stopCtx, st.RunnerName)
	return runErr
}

// deregister removes a registration that an ephemeral runner should have
// removed itself. A hard kill leaves it behind, and the next online/offline
// cycle keeps it in the list forever.
func (s *Supervisor) deregister(ctx context.Context, name string) {
	if !s.ownsRunner(name) {
		s.opts.Log.Warn("refusing to deregister a name this instance did not mint", "runner", name)
		return
	}
	runners, err := s.opts.Client.ListRunners(ctx, s.scope.APIPath())
	if err != nil {
		s.opts.Log.Warn("deregister: list runners failed", "runner", name, "err", err)
		return
	}
	for _, r := range runners {
		if r.Name != name {
			continue
		}
		if err := s.opts.Client.DeleteRunner(ctx, s.scope.APIPath(), r.ID); err != nil {
			s.opts.Log.Warn("deregister: delete failed", "runner", name, "id", r.ID, "err", err)
			return
		}
		s.opts.Log.Info("deregistered stale runner", "runner", name, "id", r.ID)
	}
}

func (s *Supervisor) draining() bool {
	_, err := os.Stat(s.drain)
	return err == nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// maxWatchdogRepos bounds how many repositories one check walks. GitHub has no
// endpoint that lists the jobs of an organization, so an org-scoped runner
// needs one runs call per repository. An org larger than this gets no
// watchdog: guessing would stop a lane that is running a job.
const maxWatchdogRepos = 100

// checkStale stops a lane that GitHub reports busy while no in-progress job
// claims its runner, and a lane whose registration is gone while no job claims
// it. A cancelled job can leave a detached process tree behind; the runner then
// waits for that tree forever, so the lane never takes new work and the tree
// keeps its memory. Stopping the scope kills the whole cgroup, which is the
// only thing that reaches a detached grandchild.
func (s *Supervisor) checkStale(ctx context.Context, now time.Time) {
	if s.staleAfter <= 0 {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	runners, err := s.opts.Client.ListRunners(checkCtx, s.scope.APIPath())
	if err != nil {
		// An incomplete answer must not stop a lane: a wrong stop fails a
		// running job.
		s.opts.Log.Warn("stale check skipped: list runners failed", "err", err)
		return
	}
	// busy is false for a runner that is registered and waiting for work, and
	// absent for a runner whose registration is gone.
	busy := make(map[string]bool, len(runners))
	for _, r := range runners {
		busy[r.Name] = r.Busy
	}

	// Only lanes that GitHub is not demonstrably using can be stale. The
	// expensive half of the check runs only when one of those exists.
	units := s.units()
	var candidates map[int]lane.State
	for idx, unit := range units {
		if idx < 0 || idx >= len(s.lanes) {
			continue
		}
		st, err := s.lanes[idx].LoadState()
		if err != nil || st.Unit != unit {
			// No state file while a scope runs means the attempt is between
			// moments: written before registering, cleared after. Leave it.
			s.clearMiss(idx)
			continue
		}
		if b, ok := busy[st.RunnerName]; ok && !b {
			s.clearMiss(idx) // registered and waiting for a job: normal
			continue
		}
		if candidates == nil {
			candidates = make(map[int]lane.State)
		}
		candidates[idx] = st
	}
	if len(candidates) == 0 {
		return
	}

	active, err := s.activeRunnerNames(checkCtx)
	if err != nil {
		s.opts.Log.Warn("stale check skipped: cannot read in-progress jobs", "err", err)
		return
	}

	for idx, st := range candidates {
		if active[st.RunnerName] {
			s.clearMiss(idx)
			continue
		}
		first, seen := s.missedAt(idx)
		if !seen {
			s.markMiss(idx, now)
			continue
		}
		if now.Sub(first) < s.staleAfter {
			continue
		}
		reason := "busy with no job"
		if _, ok := busy[st.RunnerName]; !ok {
			reason = "registration gone"
		}
		s.opts.Log.Warn("stopping a lane GitHub has no job for",
			"lane", s.lanes[idx].Name(), "runner", st.RunnerName, "unit", st.Unit,
			"reason", reason, "for", now.Sub(first).Round(time.Second).String())
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = s.lanes[idx].Stop(stopCtx, st.Unit)
		stopCancel()
		s.opts.Metrics.Add("runlet_lane_stale_stops_total", map[string]string{"lane": s.lanes[idx].Name()}, 1)
		s.clearMiss(idx)
	}
}

// activeRunnerNames asks GitHub which runners are executing a job right now.
func (s *Supervisor) activeRunnerNames(ctx context.Context) (map[string]bool, error) {
	slugs, err := s.watchRepos(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool)
	for _, slug := range slugs {
		runs, err := s.opts.Client.ListRuns(ctx, slug, policy.StatusInProgress)
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			jobs, err := s.opts.Client.ListRunJobs(ctx, slug, r.ID)
			if err != nil {
				return nil, err
			}
			for _, j := range jobs {
				if j.RunnerName != "" && j.Status == policy.StatusInProgress {
					out[j.RunnerName] = true
				}
			}
		}
	}
	return out, nil
}

// watchRepos lists the repositories a check must walk. An incomplete list is an
// error, not a shorter list: a repository the check cannot see looks exactly
// like a lane GitHub forgot.
func (s *Supervisor) watchRepos(ctx context.Context) ([]string, error) {
	if s.scope.Repo != "" {
		return []string{s.scope.Owner + "/" + s.scope.Repo}, nil
	}
	repos, err := s.opts.Client.ListOrgRepos(ctx, s.scope.Owner, maxWatchdogRepos)
	if err != nil {
		return nil, err
	}
	total, err := s.opts.Client.OrgRepoCount(ctx, s.scope.Owner)
	if err != nil {
		return nil, err
	}
	if total != len(repos) {
		return nil, fmt.Errorf("organization has %d repositories, the token listed %d", total, len(repos))
	}
	return repos, nil
}

func (s *Supervisor) units() map[int]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]string, len(s.limits))
	for idx, unit := range s.limits {
		out[idx] = unit
	}
	return out
}

func (s *Supervisor) missedAt(idx int) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.miss[idx]
	return at, ok
}

func (s *Supervisor) markMiss(idx int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.miss[idx] = now
}

func (s *Supervisor) clearMiss(idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.miss, idx)
}
