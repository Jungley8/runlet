// Package policy decides which queued runs are superseded. Decide is pure: it
// takes a snapshot, a clock, and parameters, and returns actions. The network
// and the re-check live in the caller.
package policy

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jungley8/runlet/internal/model"
)

const (
	StatusQueued     = "queued"
	StatusInProgress = "in_progress"
)

// cancelableEvents is deliberately a fixed allowlist. A new event type cannot
// start cancelling runs because nobody updated a config file.
var cancelableEvents = map[string]bool{
	"pull_request": true,
	"push":         true,
}

type Params struct {
	// Grace keeps a just-created run from being cancelled before GitHub has
	// settled it. It reduces churn; it does not close the queued->in_progress
	// race. The re-check in the caller is what makes cancellation safe.
	Grace time.Duration
}

// Decision is one cancellation the policy wants. The caller must re-read the
// run and confirm it is still queued before acting.
type Decision struct {
	Repo       string
	RunID      int64
	SurvivorID int64
	Workflow   string
	Branch     string
	Event      string
	CreatedAt  time.Time
	URL        string
	Reason     string
}

// Decide returns at most one cancellation per older run in a key
// (workflow, head repository, branch, event). The newest run in each key is the
// survivor. Ties on the newest timestamp cancel nothing: without a tiebreak
// that reflects reality, guessing can kill the run that matters.
func Decide(repo string, runs []model.Run, now time.Time, p Params) []Decision {
	groups := make(map[string][]model.Run)
	for _, r := range runs {
		if !cancelableEvents[r.Event] {
			continue
		}
		if r.Status != StatusQueued && r.Status != StatusInProgress {
			continue
		}
		if r.HeadBranch == "" {
			// A tag push has no branch. Grouping those together would cancel
			// every queued release except the newest.
			continue
		}
		if r.RunAttempt > 1 {
			// A person pressed "re-run". That is an explicit instruction, and
			// its created_at still points at the original commit, so it would
			// look superseded by any later run.
			continue
		}
		k := groupKey(r)
		groups[k] = append(groups[k], r)
	}

	var out []Decision
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			if group[i].CreatedAt.Equal(group[j].CreatedAt) {
				return group[i].ID < group[j].ID
			}
			return group[i].CreatedAt.Before(group[j].CreatedAt)
		})

		survivor := group[len(group)-1]
		if group[len(group)-2].CreatedAt.Equal(survivor.CreatedAt) {
			// Two runs claim the newest slot; neither is safe to keep or drop.
			continue
		}

		for _, r := range group[:len(group)-1] {
			if r.Status != StatusQueued {
				continue
			}
			if now.Sub(r.CreatedAt) < p.Grace {
				continue
			}
			out = append(out, Decision{
				Repo:       repo,
				RunID:      r.ID,
				SurvivorID: survivor.ID,
				Workflow:   workflowName(r),
				Branch:     r.HeadBranch,
				Event:      r.Event,
				CreatedAt:  r.CreatedAt,
				URL:        r.HTMLURL,
				Reason:     "superseded by newer run " + strconv.FormatInt(survivor.ID, 10),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].RunID < out[j].RunID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func groupKey(r model.Run) string {
	key := workflowKey(r) + "\x00" + r.HeadRepo() + "\x00" + r.HeadBranch + "\x00" + r.Event
	// The same head branch can be the source of PRs against several base
	// branches. Grouping by head alone would cancel one PR's queued run
	// because another PR was created later.
	if bases := r.BaseRefs(); len(bases) > 0 {
		key += "\x00" + strings.Join(bases, ",")
	}
	return key
}

func workflowKey(r model.Run) string {
	if r.WorkflowID != 0 {
		return strconv.FormatInt(r.WorkflowID, 10)
	}
	return r.Name
}

func workflowName(r model.Run) string {
	if r.Name != "" {
		return r.Name
	}
	return strconv.FormatInt(r.WorkflowID, 10)
}

// Snapshot is the replay format for `runlet policy check`.
type Snapshot struct {
	Now   time.Time      `yaml:"now" json:"now"`
	Repos []RepoSnapshot `yaml:"repos" json:"repos"`
}

type RepoSnapshot struct {
	Slug string      `yaml:"slug" json:"slug"`
	Runs []model.Run `yaml:"runs" json:"runs"`
}
