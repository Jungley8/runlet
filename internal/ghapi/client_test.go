package ghapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestListRunsUsesETag(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, `{"total_count":1,"workflow_runs":[
			{"id":7,"name":"CI","workflow_id":3,"head_branch":"main","event":"push",
			 "status":"queued","created_at":"2026-11-14T12:00:00Z","html_url":"https://example/7"}]}`)
	}))
	defer srv.Close()

	c := New("tok", WithBaseURL(srv.URL))
	first, err := c.ListRuns(context.Background(), "o/r", "queued")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != 7 {
		t.Fatalf("first = %+v", first)
	}

	second, err := c.ListRuns(context.Background(), "o/r", "queued")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != 7 {
		t.Fatalf("second (304) = %+v", second)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

// TestRejectsCleartextBase: a bearer token must not travel over plain http
// except to loopback, where the tests themselves live.
func TestRejectsCleartextBase(t *testing.T) {
	c := New("tok", WithBaseURL("http://api.example.com"))
	if _, err := c.ListRuns(context.Background(), "o/r", "queued"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("want an https error, got %v", err)
	}
}

func TestCancelRun(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := New("tok", WithBaseURL(srv.URL))
	if err := c.CancelRun(context.Background(), "o/r", 7); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/repos/o/r/actions/runs/7/cancel" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
}

func TestRateLimitErrors(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		headers     map[string]string
		wantRetry   time.Duration
		wantLimited bool
	}{
		{"403 primary", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, 0, true},
		{"429 secondary", http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, 7 * time.Second, true},
		{"plain 403", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "42"}, 0, false},
		{"403 without a budget header", http.StatusForbidden, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"message":"nope"}`)
			}))
			defer srv.Close()

			_, err := New("tok", WithBaseURL(srv.URL)).ListRuns(context.Background(), "o/r", "queued")
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.RateLimited() != tc.wantLimited {
				t.Fatalf("RateLimited = %v, want %v", apiErr.RateLimited(), tc.wantLimited)
			}
			if apiErr.RetryAfter != tc.wantRetry {
				t.Fatalf("RetryAfter = %s, want %s", apiErr.RetryAfter, tc.wantRetry)
			}
		})
	}
}
