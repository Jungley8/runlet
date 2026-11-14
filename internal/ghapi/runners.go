package ghapi

import (
	"context"
	"fmt"

	"github.com/Jungley8/runlet/internal/model"
)

// ListRunners returns the registrations of one organization or repository.
// scopePath is config.Scope.APIPath(), e.g. /orgs/acme/actions/runners.
func (c *Client) ListRunners(ctx context.Context, scopePath string) ([]model.Runner, error) {
	var all []model.Runner
	for page := 1; page <= 5; page++ {
		var resp struct {
			TotalCount int            `json:"total_count"`
			Runners    []model.Runner `json:"runners"`
		}
		if err := c.getJSON(ctx, fmt.Sprintf("%s?per_page=100&page=%d", scopePath, page), &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Runners...)
		if len(resp.Runners) < 100 || len(all) >= resp.TotalCount {
			break
		}
	}
	return all, nil
}

// CreateRegistrationToken mints a token that registers exactly one runner.
// GitHub expires it after an hour; a lane uses it immediately and never stores
// it.
func (c *Client) CreateRegistrationToken(ctx context.Context, scopePath string) (model.RegistrationToken, error) {
	var out model.RegistrationToken
	if err := c.postJSON(ctx, scopePath+"/registration-token", &out); err != nil {
		return model.RegistrationToken{}, err
	}
	return out, nil
}

// DeleteRunner removes a registration. It is the last step of cleanup, and it
// is safe to call twice: a missing runner answers 404, which the caller treats
// as already gone.
func (c *Client) DeleteRunner(ctx context.Context, scopePath string, id int64) error {
	return c.delete(ctx, fmt.Sprintf("%s/%d", scopePath, id))
}

// ListRunJobs finds the jobs of one run so a supervisor can report which job a
// dead lane was executing.
func (c *Client) ListRunJobs(ctx context.Context, slug string, runID int64) ([]model.Job, error) {
	var resp struct {
		TotalCount int         `json:"total_count"`
		Jobs       []model.Job `json:"jobs"`
	}
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?per_page=100", slug, runID)
	if err := c.getJSON(ctx, path, &resp); err != nil {
		return nil, err
	}
	return resp.Jobs, nil
}
