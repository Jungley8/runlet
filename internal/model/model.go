// Package model holds the GitHub Actions shapes that more than one package
// needs. It exists so the API client and the decision engine can agree on a
// type without importing each other.
package model

import (
	"sort"
	"time"
)

// Run is the subset of a workflow run that runlet reads. The JSON tags match
// the REST API; the YAML tags let a recorded snapshot be replayed.
type Run struct {
	ID         int64     `json:"id" yaml:"id"`
	Name       string    `json:"name" yaml:"name"`
	WorkflowID int64     `json:"workflow_id" yaml:"workflow_id"`
	HeadBranch string    `json:"head_branch" yaml:"head_branch"`
	Event      string    `json:"event" yaml:"event"`
	Status     string    `json:"status" yaml:"status"`
	CreatedAt  time.Time `json:"created_at" yaml:"created_at"`
	HeadSHA    string    `json:"head_sha" yaml:"head_sha"`
	HTMLURL    string    `json:"html_url" yaml:"html_url"`
	// RunAttempt counts re-runs. A human pressed that button, so attempt > 1 is
	// never a cancellation target.
	RunAttempt int `json:"run_attempt,omitempty" yaml:"run_attempt,omitempty"`
	// HeadRepository is the fork a pull_request came from. Two forks can both
	// have a branch called "main"; without this, they collide in the group key.
	HeadRepository *Repository `json:"head_repository,omitempty" yaml:"head_repository,omitempty"`
	// RunnerName is set on job objects, not on runs. It is kept here so the
	// lane supervisor can find the job assigned to a runner that died.
	RunnerName string `json:"runner_name,omitempty" yaml:"runner_name,omitempty"`
	// PullRequests is the list of pull requests a run belongs to. One head
	// branch can feed several PRs with different base branches; without the
	// base they collide in the group key and one is wrongly cancelled.
	PullRequests []PullRequest `json:"pull_requests,omitempty" yaml:"pull_requests,omitempty"`
	// Repository is the repository the run belongs to. An org-level listing
	// returns runs from many repositories, and the jobs endpoint needs one.
	Repository *Repository `json:"repository,omitempty" yaml:"repository,omitempty"`
}

// PullRequest is the slice of a workflow run's pull_requests entry that runlet
// reads.
type PullRequest struct {
	Number int        `json:"number" yaml:"number"`
	Base   *Reference `json:"base,omitempty" yaml:"base,omitempty"`
}

// Reference is a git ref object. Only the branch name is read.
type Reference struct {
	Ref string `json:"ref" yaml:"ref"`
}

// BaseRefs returns the distinct base branches, sorted. An empty result means
// the API omitted the field, which is treated as "no base information".
func (r Run) BaseRefs() []string {
	seen := make(map[string]bool, len(r.PullRequests))
	var out []string
	for _, pr := range r.PullRequests {
		if pr.Base == nil || pr.Base.Ref == "" || seen[pr.Base.Ref] {
			continue
		}
		seen[pr.Base.Ref] = true
		out = append(out, pr.Base.Ref)
	}
	sort.Strings(out)
	return out
}

// Repository identifies a fork or the base repository.
type Repository struct {
	FullName string `json:"full_name" yaml:"full_name"`
}

// HeadRepo returns the head repository's full name, or "" when the API did not
// include one.
func (r Run) HeadRepo() string {
	if r.HeadRepository == nil {
		return ""
	}
	return r.HeadRepository.FullName
}

// RepoFullName returns the repository the run belongs to, or "" when the API
// did not include one.
func (r Run) RepoFullName() string {
	if r.Repository == nil {
		return ""
	}
	return r.Repository.FullName
}

// Runner is a self-hosted runner registration.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // online | offline
	Busy   bool   `json:"busy"`
}

// RegistrationToken registers exactly one runner and expires after an hour.
type RegistrationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Job is one job of a run. runner_name is how a supervisor learns which lane
// was executing a job when that lane died.
type Job struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	RunnerName string `json:"runner_name"`
	HTMLURL    string `json:"html_url"`
}
