// Package serve runs the P1 control loop: poll each repository, decide which
// queued runs are superseded, and optionally cancel them. It does not manage
// runner lanes; that is P2.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Jungley8/runlet/internal/config"
	"github.com/Jungley8/runlet/internal/ghapi"
	"github.com/Jungley8/runlet/internal/metrics"
	"github.com/Jungley8/runlet/internal/policy"
)

type Options struct {
	Config     *config.Config
	Client     *ghapi.Client
	Metrics    *metrics.Metrics
	Log        *slog.Logger
	KillSwitch string
}

type Server struct {
	opts    Options
	limiter *limiter

	mu       sync.Mutex
	lastPoll time.Time
	lastErr  string
	repos    map[string]repoStatus
	cooldown map[string]time.Time
}

type repoStatus struct {
	Queued    int   `json:"queued"`
	Running   int   `json:"running"`
	Oldest    int64 `json:"oldest_queued_seconds"`
	Cancelled int   `json:"cancelled_last_poll"`
}

func New(opts Options) *Server {
	if opts.Metrics == nil {
		opts.Metrics = metrics.New()
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Server{
		opts:     opts,
		limiter:  &limiter{hits: make(map[string][]time.Time), cap: opts.Config.Policy.CancelCap()},
		repos:    make(map[string]repoStatus),
		cooldown: make(map[string]time.Time),
	}
}

// Serve starts the HTTP endpoint and polls until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", s.opts.Metrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/status", s.handleStatus)

	srv := &http.Server{
		Addr:              s.opts.Config.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		s.opts.Log.Info("http listening", "addr", s.opts.Config.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	s.opts.Metrics.Set("runlet_up", nil, 1)
	s.poll(ctx)

	ticker := time.NewTicker(s.opts.Config.Policy.PollEvery())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
			return nil
		case err := <-errCh:
			return err
		case <-ticker.C:
			s.poll(ctx)
		}
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := struct {
		Mode     string                `json:"mode"`
		LastPoll time.Time             `json:"last_poll"`
		LastErr  string                `json:"last_error,omitempty"`
		Repos    map[string]repoStatus `json:"repos"`
	}{
		Mode:     s.opts.Config.Policy.ModeValue(),
		LastPoll: s.lastPoll,
		LastErr:  s.lastErr,
		Repos:    s.repos,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) poll(ctx context.Context) {
	now := time.Now()
	params := policy.Params{Grace: s.opts.Config.Policy.Grace()}

	paused := false
	if s.opts.KillSwitch != "" {
		if _, err := os.Stat(s.opts.KillSwitch); err == nil {
			paused = true
			s.opts.Log.Warn("kill switch present; cancelling is paused", "path", s.opts.KillSwitch)
		}
	}
	s.opts.Metrics.Set("runlet_policy_paused", nil, boolFloat(paused))
	if paused {
		s.finishPoll(now, "")
		return
	}

	var lastErr string
	for _, repo := range s.opts.Config.Repos {
		if !repo.CancelSuperseded {
			continue
		}
		if until, ok := s.coolingDown(repo.Slug, now); ok {
			s.opts.Log.Debug("repo is backing off after a rate limit", "repo", repo.Slug, "until", until)
			continue
		}
		if err := s.pollRepo(ctx, repo.Slug, now, params); err != nil {
			lastErr = err.Error()
			s.opts.Log.Warn("poll failed", "repo", repo.Slug, "err", err)
			s.opts.Metrics.Add("runlet_policy_errors_total", map[string]string{"repo": repo.Slug}, 1)

			var apiErr *ghapi.APIError
			if errors.As(err, &apiErr) && apiErr.RateLimited() {
				wait := apiErr.RetryAfter
				if wait <= 0 {
					wait = time.Minute
				}
				s.setCooldown(repo.Slug, now.Add(wait))
				s.opts.Log.Warn("rate limited; pausing this repo", "repo", repo.Slug, "for", wait)
			}
		}
	}
	s.finishPoll(now, lastErr)
}

func (s *Server) pollRepo(ctx context.Context, slug string, now time.Time, params policy.Params) error {
	queued, err := s.opts.Client.ListRuns(ctx, slug, policy.StatusQueued)
	if err != nil {
		return fmt.Errorf("list queued: %w", err)
	}
	running, err := s.opts.Client.ListRuns(ctx, slug, policy.StatusInProgress)
	if err != nil {
		// A failed second query still leaves queued runs usable for decisions.
		s.opts.Log.Warn("list in_progress failed; deciding on queued only", "repo", slug, "err", err)
		running = nil
	}

	var oldest int64
	for _, r := range queued {
		if age := int64(now.Sub(r.CreatedAt).Seconds()); age > oldest {
			oldest = age
		}
	}
	s.opts.Metrics.Set("runlet_policy_queue_depth", map[string]string{"repo": slug}, float64(len(queued)))
	s.opts.Metrics.Set("runlet_policy_oldest_queued_seconds", map[string]string{"repo": slug}, float64(oldest))
	s.setRepoStatus(slug, repoStatus{Queued: len(queued), Running: len(running), Oldest: oldest})

	decisions := policy.Decide(slug, append(queued, running...), now, params)
	cancelled := 0
	for _, d := range decisions {
		s.opts.Metrics.Add("runlet_policy_decisions_total",
			map[string]string{"repo": slug, "mode": s.opts.Config.Policy.ModeValue()}, 1)

		if s.opts.Config.Policy.ModeValue() == "dry-run" {
			s.opts.Log.Info("policy: would cancel",
				"repo", d.Repo, "run", d.RunID, "branch", d.Branch,
				"workflow", d.Workflow, "reason", d.Reason, "url", d.URL)
			continue
		}

		if !s.limiter.allow(slug, now) {
			s.opts.Log.Warn("cancel cap reached; skipping", "repo", slug, "cap", s.opts.Config.Policy.CancelCap())
			continue
		}

		// Re-read both sides. Decide ran on a snapshot: the target may have
		// started, and the survivor may have finished, failed, or been deleted.
		// Cancelling the target then would leave the branch with no queued run.
		survivor, err := s.opts.Client.GetRun(ctx, slug, d.SurvivorID)
		if err != nil {
			s.opts.Log.Warn("survivor re-read failed; not cancelling", "repo", slug, "run", d.RunID, "survivor", d.SurvivorID, "err", err)
			continue
		}
		if survivor.Status != policy.StatusQueued && survivor.Status != policy.StatusInProgress {
			s.opts.Log.Info("skip: survivor is no longer active", "repo", slug, "survivor", d.SurvivorID, "status", survivor.Status)
			continue
		}

		current, err := s.opts.Client.GetRun(ctx, slug, d.RunID)
		if err != nil {
			s.opts.Log.Warn("re-read failed; not cancelling", "repo", slug, "run", d.RunID, "err", err)
			continue
		}
		if current.Status != policy.StatusQueued {
			s.opts.Log.Info("skip: run is no longer queued", "repo", slug, "run", d.RunID, "status", current.Status)
			continue
		}

		if err := s.opts.Client.CancelRun(ctx, slug, d.RunID); err != nil {
			var apiErr *ghapi.APIError
			if errors.As(err, &apiErr) && apiErr.RateLimited() {
				return fmt.Errorf("rate limited while cancelling: %w", err)
			}
			s.opts.Log.Warn("cancel failed", "repo", slug, "run", d.RunID, "err", err)
			continue
		}

		cancelled++
		s.opts.Metrics.Add("runlet_policy_cancellations_total", map[string]string{"repo": slug}, 1)
		s.opts.Log.Info("cancelled superseded run",
			"repo", slug, "run", d.RunID, "survivor", d.SurvivorID,
			"branch", d.Branch, "workflow", d.Workflow, "url", d.URL)
	}

	s.setRepoStatus(slug, repoStatus{Queued: len(queued), Running: len(running), Oldest: oldest, Cancelled: cancelled})
	return nil
}

func (s *Server) setRepoStatus(slug string, st repoStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[slug] = st
}

func (s *Server) coolingDown(slug string, now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.cooldown[slug]
	if !ok {
		return time.Time{}, false
	}
	if !now.Before(until) {
		delete(s.cooldown, slug)
		return time.Time{}, false
	}
	return until, true
}

func (s *Server) setCooldown(slug string, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cooldown[slug] = until
}

func (s *Server) finishPoll(now time.Time, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPoll = now
	s.lastErr = lastErr
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	cap  int
}

func (l *limiter) allow(repo string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-time.Hour)
	hits := l.hits[repo]
	i := 0
	for ; i < len(hits); i++ {
		if hits[i].After(cutoff) {
			break
		}
	}
	hits = hits[i:]

	if len(hits) >= l.cap {
		l.hits[repo] = hits
		return false
	}
	l.hits[repo] = append(hits, now)
	return true
}
