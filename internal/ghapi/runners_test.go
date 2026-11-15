package ghapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCreateRegistrationToken(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_, _ = io.WriteString(w, `{"token":"reg","expires_at":"2026-11-14T13:00:00Z"}`)
	}))
	defer srv.Close()

	tok, err := New("tok", WithBaseURL(srv.URL)).CreateRegistrationToken(context.Background(), "/orgs/acme/actions/runners")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "reg" {
		t.Fatalf("token = %q", tok.Token)
	}
	if gotMethod != http.MethodPost || gotPath != "/orgs/acme/actions/runners/registration-token" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
}

func TestListRunnersPaginates(t *testing.T) {
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		if strings.HasSuffix(r.URL.Path, "/registration-token") {
			return
		}
		_, _ = io.WriteString(w, `{"total_count":2,"runners":[
			{"id":1,"name":"runlet-box-lane-0-a","status":"offline","busy":false},
			{"id":2,"name":"other","status":"online","busy":true}]}`)
	}))
	defer srv.Close()

	runners, err := New("tok", WithBaseURL(srv.URL)).ListRunners(context.Background(), "/repos/o/r/actions/runners")
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 2 || runners[0].Name != "runlet-box-lane-0-a" {
		t.Fatalf("runners = %+v", runners)
	}
	if pages.Load() != 1 {
		t.Fatalf("pages = %d, want 1 (fewer than 100 results)", pages.Load())
	}
}

// TestListRunnersRefusesAnIncompleteList: the stale check reads a missing name
// as "GitHub has no such runner". A page the client could not read would then
// look like a lane to stop.
func TestListRunnersRefusesAnIncompleteList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"total_count":250,"runners":[
			{"id":1,"name":"runlet-box-lane-0-a","status":"offline","busy":false}]}`)
	}))
	defer srv.Close()

	_, err := New("tok", WithBaseURL(srv.URL)).ListRunners(context.Background(), "/orgs/acme/actions/runners")
	if err == nil {
		t.Fatal("a list shorter than total_count must be an error")
	}
}

func TestDeleteRunner(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := New("tok", WithBaseURL(srv.URL)).DeleteRunner(context.Background(), "/orgs/acme/actions/runners", 42); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/orgs/acme/actions/runners/42" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
}

func TestListRunJobs(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"total_count":1,"jobs":[
			{"id":9,"run_id":5,"name":"build","status":"in_progress","runner_name":"runlet-box-lane-0-a","html_url":"https://example/9"}]}`)
	}))
	defer srv.Close()

	jobs, err := New("tok", WithBaseURL(srv.URL)).ListRunJobs(context.Background(), "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].RunnerName != "runlet-box-lane-0-a" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if gotPath != "/repos/o/r/actions/runs/5/jobs" {
		t.Fatalf("path = %s", gotPath)
	}
}
