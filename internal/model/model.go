// Package model holds the GitHub Actions shapes that more than one package
// needs. It exists so the API client and the decision engine can agree on a
// type without importing each other.
package model

import "time"

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

// Runner is a self-hosted runner registration.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // online | offline
	Busy   bool   `json:"busy"`
}
