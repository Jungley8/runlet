package policy

import (
	"fmt"
	"testing"
	"time"

	"github.com/Jungley8/runlet/internal/model"
)

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func run(id int64, wf int64, branch, event, status string, age time.Duration) model.Run {
	return model.Run{
		ID:         id,
		Name:       fmt.Sprintf("wf-%d", wf),
		WorkflowID: wf,
		HeadBranch: branch,
		Event:      event,
		Status:     status,
		CreatedAt:  now.Add(-age),
	}
}

func withHeadRepo(r model.Run, full string) model.Run {
	r.HeadRepository = &model.Repository{FullName: full}
	return r
}

func withAttempt(r model.Run, n int) model.Run {
	r.RunAttempt = n
	return r
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		runs []model.Run
		want []int64
	}{
		{
			name: "superseded burst",
			runs: []model.Run{
				run(1, 10, "feat", "pull_request", StatusQueued, 10*time.Minute),
				run(2, 10, "feat", "pull_request", StatusQueued, 5*time.Minute),
				run(3, 10, "feat", "pull_request", StatusQueued, 1*time.Minute),
			},
			want: []int64{1, 2},
		},
		{
			name: "older run is running",
			runs: []model.Run{
				run(1, 10, "feat", "pull_request", StatusInProgress, 10*time.Minute),
				run(2, 10, "feat", "pull_request", StatusQueued, 1*time.Minute),
			},
			want: nil,
		},
		{
			name: "survivor is running, older queued",
			runs: []model.Run{
				run(1, 10, "feat", "pull_request", StatusQueued, 10*time.Minute),
				run(2, 10, "feat", "pull_request", StatusInProgress, 1*time.Minute),
			},
			want: []int64{1},
		},
		{
			name: "different workflows",
			runs: []model.Run{
				run(1, 10, "feat", "pull_request", StatusQueued, 10*time.Minute),
				run(2, 11, "feat", "pull_request", StatusQueued, 1*time.Minute),
			},
			want: nil,
		},
		{
			name: "different branches",
			runs: []model.Run{
				run(1, 10, "feat-a", "pull_request", StatusQueued, 10*time.Minute),
				run(2, 10, "feat-b", "pull_request", StatusQueued, 1*time.Minute),
			},
			want: nil,
		},
		{
			name: "schedule is never cancelled",
			runs: []model.Run{
				run(1, 10, "main", "schedule", StatusQueued, 10*time.Minute),
				run(2, 10, "main", "schedule", StatusQueued, 1*time.Minute),
			},
			want: nil,
		},
		{
			name: "dispatch is never cancelled",
			runs: []model.Run{
				run(1, 10, "main", "workflow_dispatch", StatusQueued, 10*time.Minute),
				run(2, 10, "main", "workflow_dispatch", StatusQueued, 1*time.Minute),
			},
			want: nil,
		},
		{
			name: "tie on newest timestamp cancels nothing",
			runs: []model.Run{
				run(1, 10, "feat", "pull_request", StatusQueued, 10*time.Minute),
				run(2, 10, "feat", "pull_request", StatusQueued, 1*time.Minute),
				run(3, 10, "feat", "pull_request", StatusQueued, 1*time.Minute),
			},
			want: nil,
		},
		{
			name: "younger than grace",
			runs: []model.Run{
				run(1, 10, "feat", "pull_request", StatusQueued, 30*time.Second),
				run(2, 10, "feat", "pull_request", StatusQueued, 10*time.Second),
			},
			want: nil,
		},
		{
			name: "single run",
			runs: []model.Run{
				run(1, 10, "feat", "pull_request", StatusQueued, 10*time.Minute),
			},
			want: nil,
		},
		{
			name: "main push burst",
			runs: []model.Run{
				run(1, 20, "main", "push", StatusQueued, 30*time.Minute),
				run(2, 20, "main", "push", StatusQueued, 2*time.Minute),
			},
			want: []int64{1},
		},
		{
			name: "fork pull requests with the same branch name do not collide",
			runs: []model.Run{
				withHeadRepo(run(1, 10, "main", "pull_request", StatusQueued, 10*time.Minute), "fork-a/x"),
				withHeadRepo(run(2, 10, "main", "pull_request", StatusQueued, 5*time.Minute), "fork-b/x"),
			},
			want: nil,
		},
		{
			name: "tag pushes have no branch and are never cancelled",
			runs: []model.Run{
				run(1, 10, "", "push", StatusQueued, 10*time.Minute),
				run(2, 10, "", "push", StatusQueued, 5*time.Minute),
			},
			want: nil,
		},
		{
			name: "a manual re-run is never cancelled",
			runs: []model.Run{
				withAttempt(run(1, 10, "feat", "pull_request", StatusQueued, 10*time.Minute), 2),
				run(2, 10, "feat", "pull_request", StatusQueued, 5*time.Minute),
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide("o/r", tc.runs, now, Params{Grace: 45 * time.Second})
			var ids []int64
			for _, d := range got {
				ids = append(ids, d.RunID)
			}
			if len(ids) != len(tc.want) {
				t.Fatalf("got %v, want %v", ids, tc.want)
			}
			for i := range ids {
				if ids[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", ids, tc.want)
				}
			}
		})
	}
}

// TestDecideNeverTargetsSurvivor is the property that matters: for every key,
// the newest run is never in the decision set.
func TestDecideNeverTargetsSurvivor(t *testing.T) {
	var runs []model.Run
	id := int64(1)
	for wf := int64(1); wf <= 3; wf++ {
		for b := 0; b < 4; b++ {
			branch := fmt.Sprintf("b%d", b)
			for i := 0; i < 5; i++ {
				status := StatusQueued
				if i == 4 {
					status = StatusInProgress
				}
				runs = append(runs, run(id, wf, branch, "pull_request", status, time.Duration(10-i*2)*time.Minute))
				id++
			}
		}
	}

	decisions := Decide("o/r", runs, now, Params{Grace: 45 * time.Second})
	targeted := make(map[int64]bool, len(decisions))
	for _, d := range decisions {
		targeted[d.RunID] = true
		if d.SurvivorID == d.RunID {
			t.Fatalf("decision targets its own survivor: %+v", d)
		}
	}

	survivors := make(map[string]int64)
	for _, r := range runs {
		if !targeted[r.ID] {
			key := groupKey(r)
			if prev, ok := survivors[key]; ok && prev != r.ID {
				// An older in_progress run is a legitimate second survivor.
				if r.Status == StatusQueued {
					t.Fatalf("two queued survivors in key %q: %d and %d", key, prev, r.ID)
				}
			}
			survivors[key] = r.ID
		}
	}
}
