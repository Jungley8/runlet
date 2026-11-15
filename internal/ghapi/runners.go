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

// ListOrgRepos returns the full names of the organization's repositories that
// the token can see, up to limit. GitHub has no endpoint that lists the jobs of
// an organization, so a caller that needs them has to walk the repositories.
func (c *Client) ListOrgRepos(ctx context.Context, owner string, limit int) ([]string, error) {
	var out []string
	for page := 1; len(out) < limit; page++ {
		var resp []struct {
			FullName string `json:"full_name"`
		}
		path := fmt.Sprintf("/orgs/%s/repos?type=all&per_page=100&page=%d", owner, page)
		if err := c.getJSON(ctx, path, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp {
			out = append(out, r.FullName)
		}
		if len(resp) < 100 {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// OrgRepoCount returns how many repositories the organization has. A caller
// that listed fewer than this saw an incomplete picture.
func (c *Client) OrgRepoCount(ctx context.Context, owner string) (int, error) {
	var org struct {
		PublicRepos       int `json:"public_repos"`
		TotalPrivateRepos int `json:"total_private_repos"`
	}
	if err := c.getJSON(ctx, "/orgs/"+owner, &org); err != nil {
		return 0, err
	}
	return org.PublicRepos + org.TotalPrivateRepos, nil
}
