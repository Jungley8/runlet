package serve

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Jungley8/runlet/internal/config"
	"github.com/Jungley8/runlet/internal/ghapi"
	"github.com/Jungley8/runlet/internal/metrics"
)

// fakeGH serves the three endpoints the policy loop touches and records every
// cancel. runByID is what a re-read sees, so a test can make a run change state
// between the list call and the cancel.
type fakeGH struct {
	mu           sync.Mutex
	queued       []map[string]any
	running      []map[string]any
	runByID      map[int64]map[string]any
	cancels      []int64
	cancelStatus int
}

func (f *fakeGH) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/actions/runs":
			switch r.URL.Query().Get("status") {
			case "queued":
				writeRuns(w, f.queued)
			case "in_progress":
				writeRuns(w, f.running)
			}
		case r.Method == http.MethodGet && len(r.URL.Path) > len("/repos/o/r/actions/runs/"):
			id := parseInt(r.URL.Path[len("/repos/o/r/actions/runs/"):])
			run, ok := f.runByID[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			writeJSON(w, run)
		case r.Method == http.MethodPost:
			id := parseInt(r.URL.Path[len("/repos/o/r/actions/runs/"):])
			f.cancels = append(f.cancels, id)
			status := f.cancelStatus
			if status == 0 {
				status = http.StatusAccepted
			}
			w.WriteHeader(status)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func writeRuns(w http.ResponseWriter, runs []map[string]any) {
	if runs == nil {
		runs = []map[string]any{}
	}
	writeJSON(w, map[string]any{"total_count": len(runs), "workflow_runs": runs})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func parseInt(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func runJSON(id int64, status string, age time.Duration) map[string]any {
	return map[string]any{
		"id":          id,
		"name":        "CI",
		"workflow_id": 9001,
		"head_branch": "feat",
		"event":       "pull_request",
		"status":      status,
		"created_at":  time.Now().Add(-age).UTC().Format(time.RFC3339),
		"html_url":    "https://example/run",
		"run_attempt": 1,
	}
}

func newTestServer(t *testing.T, mode string, f *fakeGH) *Server {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		Server: config.ServerConfig{Listen: "127.0.0.1:0"},
		Policy: config.PolicyConfig{
			Mode:             strPtr(mode),
			CancelCapPerHour: intPtr(20),
			GraceSeconds:     intPtr(0),
		},
		Repos: []config.RepoConfig{{Slug: "o/r", CancelSuperseded: true}},
	}
	return New(Options{
		Config:  cfg,
		Client:  ghapi.New("tok", ghapi.WithBaseURL(srv.URL)),
		Metrics: metrics.New(),
		Log:     discardLogger(),
	})
}

// TestPollCancelsOnlyTheSupersededRun: two queued runs on one branch, one
// winner. Only the older run may be cancelled.
func TestPollCancelsOnlyTheSupersededRun(t *testing.T) {
	old := runJSON(1, "queued", 10*time.Minute)
	newer := runJSON(2, "queued", 2*time.Minute)
	f := &fakeGH{
		queued:  []map[string]any{old, newer},
		runByID: map[int64]map[string]any{1: old, 2: newer},
	}
	s := newTestServer(t, "apply", f)
	s.poll(context.Background())

	if len(f.cancels) != 1 || f.cancels[0] != 1 {
		t.Fatalf("cancels = %v, want [1]", f.cancels)
	}
}

// TestDryRunNeverPosts: the same snapshot in dry-run mode must cancel nothing.
func TestDryRunNeverPosts(t *testing.T) {
	old := runJSON(1, "queued", 10*time.Minute)
	newer := runJSON(2, "queued", 2*time.Minute)
	f := &fakeGH{
		queued:  []map[string]any{old, newer},
		runByID: map[int64]map[string]any{1: old, 2: newer},
	}
	s := newTestServer(t, "dry-run", f)
	s.poll(context.Background())

	if len(f.cancels) != 0 {
		t.Fatalf("cancels = %v, want none", f.cancels)
	}
}

// TestPollSkipsWhenSurvivorIsGone: the survivor finished between the list call
// and the cancel. Cancelling the older run then would leave the branch with no
// queued run at all.
func TestPollSkipsWhenSurvivorIsGone(t *testing.T) {
	old := runJSON(1, "queued", 10*time.Minute)
	newer := runJSON(2, "queued", 2*time.Minute)
	survivor := runJSON(2, "completed", 2*time.Minute)
	f := &fakeGH{
		queued:  []map[string]any{old, newer},
		runByID: map[int64]map[string]any{1: old, 2: survivor},
	}
	s := newTestServer(t, "apply", f)
	s.poll(context.Background())

	if len(f.cancels) != 0 {
		t.Fatalf("cancels = %v, want none", f.cancels)
	}
}

// TestPollSkipsWhenTargetStarted: Decide saw a queued run, the re-read sees it
// in progress. A started run is never a cancellation target.
func TestPollSkipsWhenTargetStarted(t *testing.T) {
	old := runJSON(1, "queued", 10*time.Minute)
	newer := runJSON(2, "queued", 2*time.Minute)
	started := runJSON(1, "in_progress", 10*time.Minute)
	f := &fakeGH{
		queued:  []map[string]any{old, newer},
		runByID: map[int64]map[string]any{1: started, 2: newer},
	}
	s := newTestServer(t, "apply", f)
	s.poll(context.Background())

	if len(f.cancels) != 0 {
		t.Fatalf("cancels = %v, want none", f.cancels)
	}
}

// TestPollPausesRepoOnRateLimit: a 429 pauses the repository instead of
// retrying every poll.
func TestPollPausesRepoOnRateLimit(t *testing.T) {
	old := runJSON(1, "queued", 10*time.Minute)
	newer := runJSON(2, "queued", 2*time.Minute)
	f := &fakeGH{
		queued:       []map[string]any{old, newer},
		runByID:      map[int64]map[string]any{1: old, 2: newer},
		cancelStatus: http.StatusTooManyRequests,
	}
	s := newTestServer(t, "apply", f)
	s.poll(context.Background())

	if _, ok := s.coolingDown("o/r", time.Now()); !ok {
		t.Fatal("want the repo to be cooling down after a rate limit")
	}
	if _, ok := s.coolingDown("o/r", time.Now().Add(2*time.Minute)); ok {
		t.Fatal("cooldown must expire")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }
