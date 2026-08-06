// Package membership resolves the social graph BoltQ does not own.
//
// Who matched with whom, who is in which channel, who was just removed — that
// lives in the application's database, and replicating it into the broker would
// mean two sources of truth for authorisation. Instead BoltQ asks over HTTP and
// caches the answer briefly.
package membership

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client queries an application endpoint for membership facts.
//
// Two questions are asked, on separate paths:
//
//	GET  <base>/is-member?tenant=&user=&group=   -> {"member": true}
//	GET  <base>/members?tenant=&group=           -> {"members": ["u1","u2"]}
//
// They are separate because their cost profiles differ by orders of magnitude:
// "is this one user in this group" is a point lookup on every message, while
// "list all members" is a scan run only on fan-out. Collapsing them would make
// every authorisation check pay for a full member list.
type Client struct {
	base       string
	http       *http.Client
	authHeader string
}

// Options configures the Client.
type Options struct {
	// BaseURL is the application endpoint root.
	BaseURL string
	// Timeout bounds a single request. Keep it short: this call sits inline on
	// the message send path, and a slow social-graph service must degrade into
	// a denied send rather than a hung connection.
	Timeout time.Duration
	// AuthHeader is sent verbatim as the Authorization header.
	AuthHeader string
	// MaxIdleConns tunes connection reuse.
	MaxIdleConns int
}

// New builds a membership client.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("membership: base url is required")
	}
	if _, err := url.Parse(opts.BaseURL); err != nil {
		return nil, fmt.Errorf("membership: invalid base url: %w", err)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Second
	}
	if opts.MaxIdleConns <= 0 {
		opts.MaxIdleConns = 64
	}

	return &Client{
		base:       trimSlash(opts.BaseURL),
		authHeader: opts.AuthHeader,
		http: &http.Client{
			Timeout: opts.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        opts.MaxIdleConns,
				MaxIdleConnsPerHost: opts.MaxIdleConns,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   2 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
	}, nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// IsMember implements identity.MembershipChecker.
func (c *Client) IsMember(ctx context.Context, tenant, userID, groupID string) (bool, error) {
	q := url.Values{}
	q.Set("tenant", tenant)
	q.Set("user", userID)
	q.Set("group", groupID)

	var out struct {
		Member bool `json:"member"`
	}
	if err := c.get(ctx, "/is-member?"+q.Encode(), &out); err != nil {
		return false, err
	}
	return out.Member, nil
}

// Members implements fanout.MemberLister.
func (c *Client) Members(ctx context.Context, tenant, groupID string) ([]string, error) {
	q := url.Values{}
	q.Set("tenant", tenant)
	q.Set("group", groupID)

	var out struct {
		Members []string `json:"members"`
	}
	if err := c.get(ctx, "/members?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out.Members, nil
}

func (c *Client) get(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("membership request: %w", err)
	}
	defer resp.Body.Close()

	// Cap the response read. A membership endpoint returning an unbounded body
	// — by bug or by compromise — must not be able to exhaust broker memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("membership response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("membership endpoint returned %d: %s", resp.StatusCode, truncate(body, 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("membership response is not JSON: %w", err)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// Static is an in-process member lister backed by a fixed table. It exists so
// a deployment can run — and be tested — without standing up a membership
// service first.
type Static struct {
	groups map[string][]string
}

// NewStatic builds a Static lister from a group table.
func NewStatic(groups map[string][]string) *Static {
	m := make(map[string][]string, len(groups))
	for k, v := range groups {
		m[k] = append([]string(nil), v...)
	}
	return &Static{groups: m}
}

// Members implements fanout.MemberLister.
func (s *Static) Members(_ context.Context, _, groupID string) ([]string, error) {
	return s.groups[groupID], nil
}

// IsMember implements identity.MembershipChecker.
func (s *Static) IsMember(_ context.Context, _, userID, groupID string) (bool, error) {
	for _, m := range s.groups[groupID] {
		if m == userID {
			return true, nil
		}
	}
	return false, nil
}

// DirectMembers derives the two participants of a direct conversation whose ID
// is formed by joining the sorted user IDs with a separator.
//
// This is a convention many 1:1 chat schemas already use ("alice:bob"), and
// honouring it means direct messages need no membership service at all — the
// conversation ID is the membership.
type DirectMembers struct {
	Separator string
	// Fallback handles conversation IDs that do not fit the convention, such
	// as group IDs. Optional.
	Fallback interface {
		Members(ctx context.Context, tenant, groupID string) ([]string, error)
	}
}

// Members implements fanout.MemberLister.
func (d *DirectMembers) Members(ctx context.Context, tenant, conversationID string) ([]string, error) {
	sep := d.Separator
	if sep == "" {
		sep = ":"
	}
	parts := strings.Split(conversationID, sep)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts, nil
	}
	if d.Fallback != nil {
		return d.Fallback.Members(ctx, tenant, conversationID)
	}
	return nil, nil
}

// IsMember implements identity.MembershipChecker.
func (d *DirectMembers) IsMember(ctx context.Context, tenant, userID, conversationID string) (bool, error) {
	members, err := d.Members(ctx, tenant, conversationID)
	if err != nil {
		return false, err
	}
	for _, m := range members {
		if m == userID {
			return true, nil
		}
	}
	return false, nil
}
