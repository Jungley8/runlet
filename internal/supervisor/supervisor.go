// Package supervisor runs the configured lanes: it starts each runner under a
// systemd scope, adopts scopes that survived a restart, and cleans up after
// every exit path.
package supervisor

import (
	"context"
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
	lanes  []*lane.Lane
	drain  string
	mu     sync.Mutex
	limits map[int]string // lane index -> adopted unit
}

func New(opts Options) (*Supervisor, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.New()
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
		drain:  filepath.Join(pool.StateDir, "DRAIN"),
		limits: make(map[int]string),
	}
	for i, lc := range pool.Lanes {
		s.lanes = append(s.lanes, lane.New(lane.Options{
			Index:      i,
			Name:       lc.Name,
			Labels:     lc.Labels,
			Memory:     lc.Memory.Int64(),
			MemoryHigh: lc.MemoryHigh.Int64(),
			CPUQuota:   lc.CPUQuota,
			RunnerDir:  pool.RunnerDir,
			RunnerURL:  strings.TrimSpace(pool.RunnerURL),
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

	var wg sync.WaitGroup
	for _, l := range s.lanes {
		wg.Add(1)
		go func(l *lane.Lane) {
			defer wg.Done()
			s.loop(ctx, l)
		}(l)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// adopt finds lane scopes that are still running. A supervisor restart must not
// start a second runner for the same slot: two runners with different names
// both register, and GitHub hands work to both.
func (s *Supervisor) adopt(ctx context.Context) {
	units, err := s.lanes[0].ListUnits(ctx)
	if err != nil {
		s.opts.Log.Warn("lane adoption skipped: cannot list units", "err", err)
		return
	}
	for _, u := range units {
		idx, ok := unitIndex(u)
		if !ok || idx < 0 || idx >= len(s.lanes) {
			continue
		}
		if !s.lanes[idx].IsActive(ctx, u) {
			continue
		}
		s.mu.Lock()
		s.limits[idx] = u
		s.mu.Unlock()
		s.opts.Log.Info("adopted a running lane", "lane", s.lanes[idx].Name(), "unit", u)
	}
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
		s.deregister(stopCtx, st.RunnerName)
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
	defer func() {
		_ = l.ClearState()
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
