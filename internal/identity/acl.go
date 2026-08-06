package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Action is an operation a principal may attempt on a topic.
type Action string

const (
	ActionRead   Action = "read"   // read history, tail, subscribe
	ActionWrite  Action = "write"  // append, publish
	ActionManage Action = "manage" // create, configure or delete the topic
)

// Effect is a rule's verdict.
type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
)

var (
	// ErrDenied is returned when authorisation fails. It deliberately carries
	// no detail about *why*: telling an attacker whether a conversation exists,
	// or whether they merely lack membership, is an enumeration oracle.
	ErrDenied = errors.New("identity: access denied")
	// ErrNoPrincipal means the connection was never authenticated.
	ErrNoPrincipal = errors.New("identity: not authenticated")
)

// Rule is one entry in a policy.
type Rule struct {
	// Pattern matches topic names. Segments are dot-separated. '*' matches
	// exactly one segment, '#' matches zero or more trailing segments.
	//
	// Placeholders are substituted from the principal before matching:
	//   ${user}, ${device}, ${tenant}
	// A rule of "chat.inbox.${user}.#" therefore grants each user access to
	// their own inbox and nobody else's, from a single rule.
	Pattern string `json:"pattern"`

	// ExceptPattern carves a hole in Pattern: the rule does not apply to
	// topics matching it. Placeholders are expanded the same way.
	//
	// This exists because deny-wins makes a broad deny unable to spare a
	// narrow case. "Nobody may touch an inbox" and "except your own" cannot be
	// two rules — the deny would always win. Expressed as one rule with an
	// exception, the intent survives however many allow rules follow it.
	ExceptPattern string `json:"except_pattern,omitempty"`

	// Actions this rule covers. Empty means all actions.
	Actions []Action `json:"actions,omitempty"`

	// Effect is allow or deny. Deny always wins over allow.
	Effect Effect `json:"effect"`

	// RequireScope, when non-empty, limits the rule to principals holding at
	// least one of the listed scopes.
	RequireScope []string `json:"require_scope,omitempty"`

	// RequireMembership makes the rule conditional on the principal belonging
	// to the group named by segment MembershipSegment of the topic. This is
	// what stops a user from reading a group conversation they were never
	// added to — a fact the pattern alone cannot express.
	RequireMembership bool `json:"require_membership,omitempty"`

	// MembershipSegment is the zero-based index of the topic segment holding
	// the group ID, e.g. 2 for "chat.group.<group-id>".
	MembershipSegment int `json:"membership_segment,omitempty"`
}

func (r *Rule) covers(a Action) bool {
	if len(r.Actions) == 0 {
		return true
	}
	for _, x := range r.Actions {
		if x == a {
			return true
		}
	}
	return false
}

func (r *Rule) scopeSatisfied(p *Principal) bool {
	if len(r.RequireScope) == 0 {
		return true
	}
	for _, s := range r.RequireScope {
		if p.HasScope(s) {
			return true
		}
	}
	return false
}

// MembershipChecker answers "is this user in this group?".
//
// BoltQ does not own the social graph — who matched with whom, who is in which
// Slack channel — and should not try to. The application owns it; this is the
// seam through which BoltQ asks.
type MembershipChecker interface {
	IsMember(ctx context.Context, tenant, userID, groupID string) (bool, error)
}

// MembershipFunc adapts a function to MembershipChecker.
type MembershipFunc func(ctx context.Context, tenant, userID, groupID string) (bool, error)

// IsMember implements MembershipChecker.
func (f MembershipFunc) IsMember(ctx context.Context, tenant, userID, groupID string) (bool, error) {
	return f(ctx, tenant, userID, groupID)
}

// DenyAllMembership refuses every membership question. It is the default so a
// policy referencing membership without a configured checker fails closed.
type DenyAllMembership struct{}

// IsMember implements MembershipChecker.
func (DenyAllMembership) IsMember(context.Context, string, string, string) (bool, error) {
	return false, nil
}

// StaticMembership is an in-memory group table, suitable for tests, small
// deployments, and as a cache in front of a real source of truth.
type StaticMembership struct {
	mu     sync.RWMutex
	groups map[string]map[string]bool // "tenant\x00group" -> userID set
}

// NewStaticMembership creates an empty membership table.
func NewStaticMembership() *StaticMembership {
	return &StaticMembership{groups: make(map[string]map[string]bool)}
}

func membershipKey(tenant, group string) string { return tenant + "\x00" + group }

// Add places a user in a group.
func (s *StaticMembership) Add(tenant, groupID string, userIDs ...string) {
	k := membershipKey(tenant, groupID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groups[k] == nil {
		s.groups[k] = make(map[string]bool)
	}
	for _, u := range userIDs {
		s.groups[k][u] = true
	}
}

// Remove takes a user out of a group.
func (s *StaticMembership) Remove(tenant, groupID string, userIDs ...string) {
	k := membershipKey(tenant, groupID)
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[k]
	if g == nil {
		return
	}
	for _, u := range userIDs {
		delete(g, u)
	}
	if len(g) == 0 {
		delete(s.groups, k)
	}
}

// Members lists a group's users.
func (s *StaticMembership) Members(tenant, groupID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.groups[membershipKey(tenant, groupID)]
	out := make([]string, 0, len(g))
	for u := range g {
		out = append(out, u)
	}
	return out
}

// MemberCount returns a group's size.
func (s *StaticMembership) MemberCount(tenant, groupID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.groups[membershipKey(tenant, groupID)])
}

// IsMember implements MembershipChecker.
func (s *StaticMembership) IsMember(_ context.Context, tenant, userID, groupID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.groups[membershipKey(tenant, groupID)][userID], nil
}

// CachedMembership wraps a slow MembershipChecker with a positive/negative TTL
// cache. Every message send and every subscribe asks a membership question, so
// an uncached checker turns the application's database into the bottleneck.
type CachedMembership struct {
	inner   MembershipChecker
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]cachedAnswer
	maxSize int
}

type cachedAnswer struct {
	member  bool
	expires time.Time
}

// NewCachedMembership wraps inner with a TTL cache.
//
// The TTL is the window during which a removed user can still act on a group.
// Keep it short (seconds), and have the application call Invalidate on removal
// for immediate effect.
func NewCachedMembership(inner MembershipChecker, ttl time.Duration, maxSize int) *CachedMembership {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if maxSize <= 0 {
		maxSize = 100000
	}
	return &CachedMembership{
		inner:   inner,
		ttl:     ttl,
		entries: make(map[string]cachedAnswer),
		maxSize: maxSize,
	}
}

func cacheKey(tenant, userID, groupID string) string {
	return tenant + "\x00" + userID + "\x00" + groupID
}

// IsMember implements MembershipChecker.
func (c *CachedMembership) IsMember(ctx context.Context, tenant, userID, groupID string) (bool, error) {
	k := cacheKey(tenant, userID, groupID)
	now := time.Now()

	c.mu.RLock()
	e, ok := c.entries[k]
	c.mu.RUnlock()
	if ok && now.Before(e.expires) {
		return e.member, nil
	}

	member, err := c.inner.IsMember(ctx, tenant, userID, groupID)
	if err != nil {
		return false, err
	}

	c.mu.Lock()
	// Crude bound: when full, drop everything rather than track an LRU. The
	// cache refills from the source of truth, so the cost is a latency blip,
	// and the bookkeeping an LRU would need is not worth it here.
	if len(c.entries) >= c.maxSize {
		c.entries = make(map[string]cachedAnswer, c.maxSize/2)
	}
	c.entries[k] = cachedAnswer{member: member, expires: now.Add(c.ttl)}
	c.mu.Unlock()

	return member, nil
}

// Invalidate drops a cached answer, making a membership change take effect
// immediately rather than after the TTL.
func (c *CachedMembership) Invalidate(tenant, userID, groupID string) {
	c.mu.Lock()
	delete(c.entries, cacheKey(tenant, userID, groupID))
	c.mu.Unlock()
}

// InvalidateGroup drops every cached answer about a group.
func (c *CachedMembership) InvalidateGroup(tenant, groupID string) {
	suffix := "\x00" + groupID
	prefix := tenant + "\x00"
	c.mu.Lock()
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, suffix) {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

// Policy is an ordered rule set evaluated against a principal and topic.
type Policy struct {
	mu         sync.RWMutex
	rules      []Rule
	membership MembershipChecker
	// AllowAnonymous lets shared-API-key connections bypass the policy. This
	// preserves the existing trusted-backend model while user tokens are
	// subject to the full rule set.
	allowAnonymous bool
}

// PolicyConfig configures a Policy.
type PolicyConfig struct {
	Rules          []Rule
	Membership     MembershipChecker
	AllowAnonymous bool
}

// NewPolicy builds a policy. With no rules, everything is denied — a policy
// that fails open would be worse than no policy at all, because it would look
// like protection.
func NewPolicy(cfg PolicyConfig) *Policy {
	m := cfg.Membership
	if m == nil {
		m = DenyAllMembership{}
	}
	return &Policy{
		rules:          append([]Rule(nil), cfg.Rules...),
		membership:     m,
		allowAnonymous: cfg.AllowAnonymous,
	}
}

// SetRules replaces the rule set atomically.
func (p *Policy) SetRules(rules []Rule) {
	p.mu.Lock()
	p.rules = append([]Rule(nil), rules...)
	p.mu.Unlock()
}

// Rules returns a copy of the current rule set.
func (p *Policy) Rules() []Rule {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]Rule(nil), p.rules...)
}

// SetMembership swaps the membership checker.
func (p *Policy) SetMembership(m MembershipChecker) {
	if m == nil {
		m = DenyAllMembership{}
	}
	p.mu.Lock()
	p.membership = m
	p.mu.Unlock()
}

// expand substitutes principal placeholders into a pattern.
func expand(pattern string, p *Principal) string {
	if !strings.Contains(pattern, "${") {
		return pattern
	}
	r := strings.NewReplacer(
		"${user}", p.UserID,
		"${device}", p.DeviceID,
		"${tenant}", p.Tenant,
	)
	return r.Replace(pattern)
}

// Authorize decides whether the principal may perform action on topic.
//
// Evaluation order is: expired token, then explicit deny, then explicit allow,
// then default deny. Deny is checked before allow so a narrow prohibition
// cannot be defeated by a broad permission placed after it.
func (p *Policy) Authorize(ctx context.Context, pr *Principal, topic string, action Action) error {
	if pr == nil {
		return ErrNoPrincipal
	}
	if pr.Expired(time.Now()) {
		return fmt.Errorf("%w: token expired", ErrDenied)
	}
	if pr.Anonymous {
		p.mu.RLock()
		allowed := p.allowAnonymous
		p.mu.RUnlock()
		if allowed {
			return nil
		}
		return ErrDenied
	}

	p.mu.RLock()
	rules := p.rules
	membership := p.membership
	p.mu.RUnlock()

	segments := strings.Split(topic, ".")
	var allowed bool

	for i := range rules {
		r := &rules[i]
		if !r.covers(action) {
			continue
		}
		if !topicMatch(topic, expand(r.Pattern, pr)) {
			continue
		}
		if r.ExceptPattern != "" && topicMatch(topic, expand(r.ExceptPattern, pr)) {
			continue
		}
		if !r.scopeSatisfied(pr) {
			continue
		}

		if r.RequireMembership {
			if r.MembershipSegment < 0 || r.MembershipSegment >= len(segments) {
				continue // pattern matched but the topic has no such segment
			}
			ok, err := membership.IsMember(ctx, pr.Tenant, pr.UserID, segments[r.MembershipSegment])
			if err != nil {
				// A membership backend that is down must not become an
				// authorisation bypass.
				return fmt.Errorf("%w: membership check failed: %v", ErrDenied, err)
			}
			if !ok {
				continue
			}
		}

		if r.Effect == Deny {
			return ErrDenied
		}
		allowed = true
	}

	if allowed {
		return nil
	}
	return ErrDenied
}

// Can is Authorize reduced to a boolean, for call sites that only branch.
func (p *Policy) Can(ctx context.Context, pr *Principal, topic string, action Action) bool {
	return p.Authorize(ctx, pr, topic, action) == nil
}

// topicMatch matches a dot-separated topic against a pattern where '*' matches
// one segment and '#' matches zero or more trailing segments.
func topicMatch(topic, pattern string) bool {
	if pattern == "#" {
		return true
	}
	return matchSegments(strings.Split(topic, "."), 0, strings.Split(pattern, "."), 0)
}

func matchSegments(t []string, ti int, p []string, pi int) bool {
	for pi < len(p) {
		if p[pi] == "#" {
			if pi == len(p)-1 {
				return true
			}
			for i := ti; i <= len(t); i++ {
				if matchSegments(t, i, p, pi+1) {
					return true
				}
			}
			return false
		}
		if ti >= len(t) {
			return false
		}
		if p[pi] != "*" && p[pi] != t[ti] {
			return false
		}
		ti++
		pi++
	}
	return ti == len(t)
}

// ChatPolicyRules returns a rule set implementing the access model a messaging
// app needs. It is the recommended starting point; adjust the topic names to
// match your namespace.
//
// The model:
//
//	chat.inbox.<user>.#      a user's own inbox — read/write only by that user
//	chat.direct.<convID>.#   a 1:1 conversation — membership-gated
//	chat.group.<groupID>.#   a group conversation — membership-gated
//	presence.<user>.#        a user's presence — they write, anyone reads
//	typing.<convID>.#        ephemeral typing signals — membership-gated
//	system.#                 server-originated announcements — read-only
func ChatPolicyRules() []Rule {
	return []Rule{
		// A user owns their inbox completely.
		{Pattern: "chat.inbox.${user}.#", Effect: Allow,
			Actions: []Action{ActionRead, ActionWrite}},

		// Nobody may touch another user's inbox. Stated as a deny — rather than
		// left to default-deny — so that a broad allow added later cannot
		// open it back up. The exception spares the rule's own author.
		{Pattern: "chat.inbox.#", ExceptPattern: "chat.inbox.${user}.#", Effect: Deny},

		// Conversations require membership, checked per request.
		{Pattern: "chat.direct.*.#", Effect: Allow, RequireMembership: true, MembershipSegment: 2,
			Actions: []Action{ActionRead, ActionWrite}},
		{Pattern: "chat.group.*.#", Effect: Allow, RequireMembership: true, MembershipSegment: 2,
			Actions: []Action{ActionRead, ActionWrite}},

		// Presence: a user publishes only their own, but may observe others.
		// Observing everyone is intentional — that is what a contact list is.
		{Pattern: "presence.${user}.#", Effect: Allow, Actions: []Action{ActionWrite},
			RequireScope: []string{ScopePresence}},
		{Pattern: "presence.#", Effect: Allow, Actions: []Action{ActionRead}},

		// Typing indicators follow conversation membership.
		{Pattern: "typing.*.#", Effect: Allow, RequireMembership: true, MembershipSegment: 1,
			Actions: []Action{ActionRead, ActionWrite}},

		// System announcements are read-only for users.
		{Pattern: "system.#", Effect: Allow, Actions: []Action{ActionRead}},
		{Pattern: "system.#", Effect: Deny, Actions: []Action{ActionWrite, ActionManage}},

		// Topic administration is reserved for admin-scoped principals.
		{Pattern: "#", Effect: Allow, Actions: []Action{ActionManage},
			RequireScope: []string{ScopeAdmin}},
	}
}
