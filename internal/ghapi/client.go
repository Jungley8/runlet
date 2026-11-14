// Package ghapi is the small slice of the GitHub REST API that runlet needs.
// It is hand-written net/http, like the other Go tools in this workspace.
package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jungley8/runlet/internal/model"
)

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"
	maxBodyBytes   = 8 << 20
	// maxCacheEntries bounds the ETag cache. Long-lived runs fetch one path per
	// queued run ID, so an uncapped map is an unbounded leak.
	maxCacheEntries = 512
)

type Client struct {
	base    string
	token   string
	hc      *http.Client
	baseErr error
	mu      sync.Mutex
	cache   map[string]etagEntry
}

type etagEntry struct {
	etag string
	body []byte
}

type Option func(*Client)

func WithBaseURL(u string) Option {
	return func(c *Client) { c.base = strings.TrimRight(u, "/") }
}

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.hc = h }
}

func New(token string, opts ...Option) *Client {
	c := &Client{
		base:  defaultBaseURL,
		token: token,
		hc:    &http.Client{Timeout: 20 * time.Second},
		cache: make(map[string]etagEntry),
	}
	for _, o := range opts {
		o(c)
	}
	c.baseErr = validateBase(c.base)
	return c
}

// validateBase refuses to send a bearer token in clear text. Plain http is
// allowed only for a loopback address, which is what httptest uses. Nothing in
// config can reach this today; the guard exists because P2 will let an operator
// point runlet at a GitHub Enterprise host.
func validateBase(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base url %q: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("base url %q has no host", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("base url %q must use https; plain http is allowed only for loopback", raw)
	}
	return nil
}

// APIError carries the status and the rate-limit hints. Callers use
// RateLimited() to decide whether to pause a repository instead of retrying.
type APIError struct {
	Status     int
	Message    string
	RetryAfter time.Duration
	Remaining  int
}

func (e *APIError) Error() string {
	if e.RateLimited() {
		return fmt.Sprintf("github api: rate limited (status %d, retry after %s)", e.Status, e.RetryAfter)
	}
	return fmt.Sprintf("github api: status %d: %s", e.Status, e.Message)
}

// RateLimited reports whether retrying later can help. A 403 counts only when
// GitHub says the remaining budget is zero: a permission error is also a 403,
// and backing off would hide it forever. Remaining is -1 when the header is
// absent, so an absent header is not read as exhaustion.
func (e *APIError) RateLimited() bool {
	return e.Status == http.StatusTooManyRequests ||
		(e.Status == http.StatusForbidden && e.Remaining == 0)
}

// ListRuns returns up to five pages of runs with the given status.
func (c *Client) ListRuns(ctx context.Context, slug, status string) ([]model.Run, error) {
	var all []model.Run
	for page := 1; page <= 5; page++ {
		path := fmt.Sprintf("/repos/%s/actions/runs?status=%s&per_page=100&page=%d",
			slug, url.QueryEscape(status), page)
		var resp struct {
			TotalCount   int         `json:"total_count"`
			WorkflowRuns []model.Run `json:"workflow_runs"`
		}
		if err := c.getJSON(ctx, path, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.WorkflowRuns...)
		if len(resp.WorkflowRuns) < 100 || len(all) >= resp.TotalCount {
			break
		}
	}
	return all, nil
}

// GetRun re-reads one run. The cancellation loop uses it to confirm the run is
// still queued immediately before POSTing the cancel.
func (c *Client) GetRun(ctx context.Context, slug string, id int64) (model.Run, error) {
	var run model.Run
	path := fmt.Sprintf("/repos/%s/actions/runs/%d", slug, id)
	err := c.getJSON(ctx, path, &run)
	return run, err
}

// CancelRun asks GitHub to cancel a run. 202 is the normal answer.
func (c *Client) CancelRun(ctx context.Context, slug string, id int64) error {
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", slug, id)
	return c.post(ctx, path)
}

type RateLimit struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

func (c *Client) RateLimit(ctx context.Context) (RateLimit, error) {
	var resp struct {
		Resources struct {
			Core struct {
				Limit     int   `json:"limit"`
				Remaining int   `json:"remaining"`
				Reset     int64 `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}
	if err := c.getJSON(ctx, "/rate_limit", &resp); err != nil {
		return RateLimit{}, err
	}
	return RateLimit{
		Limit:     resp.Resources.Core.Limit,
		Remaining: resp.Resources.Core.Remaining,
		Reset:     time.Unix(resp.Resources.Core.Reset, 0),
	}, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	if c.baseErr != nil {
		return c.baseErr
	}
	c.mu.Lock()
	cached, haveCache := c.cache[path]
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if haveCache && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		if !haveCache {
			return errors.New("github api: 304 without a cached body")
		}
		return json.Unmarshal(cached.body, out)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		c.mu.Lock()
		if len(c.cache) >= maxCacheEntries {
			clear(c.cache)
		}
		c.cache[path] = etagEntry{etag: etag, body: body}
		c.mu.Unlock()
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *Client) post(ctx context.Context, path string) error {
	return c.write(ctx, http.MethodPost, path, nil)
}

func (c *Client) postJSON(ctx context.Context, path string, out any) error {
	return c.write(ctx, http.MethodPost, path, out)
}

func (c *Client) delete(ctx context.Context, path string) error {
	return c.write(ctx, http.MethodDelete, path, nil)
}

func (c *Client) write(ctx context.Context, method, path string, out any) error {
	if c.baseErr != nil {
		return c.baseErr
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func parseAPIError(resp *http.Response) error {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body)

	e := &APIError{Status: resp.StatusCode, Message: body.Message, Remaining: -1}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			// Clamp: an endpoint can otherwise park a repository for years, and
			// a huge value overflows the Duration.
			if secs < 0 {
				secs = 0
			}
			if secs > 3600 {
				secs = 3600
			}
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	if rem := resp.Header.Get("X-RateLimit-Remaining"); rem != "" {
		if n, err := strconv.Atoi(rem); err == nil {
			e.Remaining = n
		}
	}
	return e
}
