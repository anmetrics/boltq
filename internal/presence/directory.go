package presence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// Presence at ten million concurrent sockets cannot be replicated everywhere.
//
// Every node knowing every session means every node holding ten million entries
// and every connect, disconnect and state change fanning out to every node —
// a load that grows with the product of users and nodes. Putting it through
// consensus is worse still: presence changes constantly and matters for
// seconds, which is the exact opposite of what a replicated log is good at.
//
// So presence is *sharded*, not replicated. A user's sessions live on one node,
// chosen by hashing the user ID, and everyone else asks that node. Each node
// holds one Nth of the state and answers one Nth of the questions.
//
// The shard map is not a new mechanism: shards are partitions of a reserved
// topic, so ownership, failover and rebalancing are the ones the control plane
// already provides. When a node dies, its presence shards fail over exactly
// like its message partitions do — and the sessions on it were lost anyway,
// because the sockets were.

// ShardTopic is the reserved topic whose partitions define presence shards.
//
// It holds no records. Only its partition assignments are used, which is what
// lets presence inherit placement and failover without inventing a second
// membership mechanism that could disagree with the first.
//
// Distinct from PresenceTopic(userID), which names the per-user pub/sub topic
// clients subscribe to. One is where presence is *announced*; this is where it
// is *stored*.
const ShardTopic = "_presence"

// ShardResolver answers which node owns a user's presence.
type ShardResolver interface {
	// OwnerOf returns the node that owns userID's shard, the address to reach
	// it, and whether that node is this one. An empty nodeID means the shard is
	// currently unowned — its previous owner failed and no successor has been
	// assigned yet.
	OwnerOf(userID string) (nodeID, addr string, local bool)
}

// Directory routes presence operations to the node owning each user's shard.
//
// Local operations go straight to the registry with no network hop, and in a
// well-balanced cluster that covers a good fraction of them: a user's own
// session is reported by the node holding their socket, which owns their shard
// only by coincidence — so most reports do travel. Reads are what this is for.
type Directory struct {
	local    *Registry
	resolver ShardResolver
	http     *http.Client
	apiKey   string

	localHits  atomic.Uint64
	remoteHits atomic.Uint64
	failures   atomic.Uint64
	unowned    atomic.Uint64
}

// DirectoryOptions configures presence routing.
type DirectoryOptions struct {
	// Registry holds the shards this node owns.
	Registry *Registry
	// Resolver maps users to owners. Nil means single-node: every user is local.
	Resolver ShardResolver
	// APIKey authenticates to peers.
	APIKey string
	// Timeout bounds one remote lookup. It sits on the message delivery path,
	// so it must be short: a presence answer that arrives late is worse than a
	// pessimistic one, because the message waits for it. Default 1s.
	Timeout time.Duration
}

// NewDirectory creates a presence directory.
func NewDirectory(opts DirectoryOptions) *Directory {
	if opts.Timeout <= 0 {
		opts.Timeout = time.Second
	}
	return &Directory{
		local:    opts.Registry,
		resolver: opts.Resolver,
		apiKey:   opts.APIKey,
		http: &http.Client{
			Timeout: opts.Timeout,
			Transport: &http.Transport{
				MaxIdleConns:        256,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// ShardForUser maps a user to a shard index.
//
// FNV-1a, the same function the stream log partitions by, so a user's presence
// shard and their inbox partition are computed identically. Reimplementing the
// hash differently here would put presence and messages on different nodes for
// the same user and double the cross-node traffic for no reason.
func ShardForUser(userID string, shards int32) int32 {
	if shards <= 1 {
		return 0
	}
	h := fnv.New64a()
	h.Write([]byte(userID))
	return int32(h.Sum64() % uint64(shards))
}

// DirectoryStats reports routing volume.
type DirectoryStats struct {
	LocalHits  uint64 `json:"local_hits"`
	RemoteHits uint64 `json:"remote_hits"`
	Failures   uint64 `json:"failures"`
	Unowned    uint64 `json:"unowned"`
}

// Stats returns routing counters.
func (d *Directory) Stats() DirectoryStats {
	return DirectoryStats{
		LocalHits:  d.localHits.Load(),
		RemoteHits: d.remoteHits.Load(),
		Failures:   d.failures.Load(),
		Unowned:    d.unowned.Load(),
	}
}

// Online reports whether a user has any live session anywhere in the cluster.
//
// On failure it answers **true**, deliberately. This call decides whether to
// send a push notification, and the two wrong answers are not equally bad: a
// false "offline" wakes someone's phone for a message they are already reading,
// while a false "online" silently drops a notification the user needed. When
// the cluster cannot answer, assume the user is reachable and let delivery
// through.
func (d *Directory) Online(ctx context.Context, userID string) bool {
	if d.resolver == nil {
		d.localHits.Add(1)
		return d.local.Online(userID)
	}

	nodeID, addr, local := d.resolver.OwnerOf(userID)
	if local {
		d.localHits.Add(1)
		return d.local.Online(userID)
	}
	if nodeID == "" || addr == "" {
		d.unowned.Add(1)
		return true
	}

	sessions, err := d.fetch(ctx, addr, userID)
	if err != nil {
		d.failures.Add(1)
		return true
	}
	d.remoteHits.Add(1)
	return len(sessions) > 0
}

// Sessions returns a user's live sessions from wherever they are held.
//
// Unlike Online, a failure returns the error: the caller is routing a delivery
// to specific nodes, and inventing sessions would send messages nowhere.
func (d *Directory) Sessions(ctx context.Context, userID string) ([]Session, error) {
	if d.resolver == nil {
		d.localHits.Add(1)
		return d.local.Sessions(userID), nil
	}

	nodeID, addr, local := d.resolver.OwnerOf(userID)
	if local {
		d.localHits.Add(1)
		return d.local.Sessions(userID), nil
	}
	if nodeID == "" || addr == "" {
		d.unowned.Add(1)
		return nil, fmt.Errorf("presence: no node owns the shard for %q", userID)
	}

	sessions, err := d.fetch(ctx, addr, userID)
	if err != nil {
		d.failures.Add(1)
		return nil, err
	}
	d.remoteHits.Add(1)
	return sessions, nil
}

// Report publishes a session to its owning shard.
//
// The node holding the socket is rarely the node owning the user's shard, so
// this usually travels. It is a fire-and-forget update on connect, disconnect
// and state change — not per message — so the volume is bounded by session
// churn rather than by traffic.
func (d *Directory) Report(ctx context.Context, s Session) error {
	if d.resolver == nil {
		_, err := d.local.Bind(s)
		return err
	}

	nodeID, addr, local := d.resolver.OwnerOf(s.UserID)
	if local {
		_, err := d.local.Bind(s)
		return err
	}
	if nodeID == "" || addr == "" {
		d.unowned.Add(1)
		// Nowhere to record it. The session still works — the socket is held
		// here and this node will deliver to it — but other nodes cannot see it
		// until the shard has an owner again.
		return fmt.Errorf("presence: no node owns the shard for %q", s.UserID)
	}
	return d.post(ctx, addr, s)
}

// LocalRegistry exposes the shards this node owns, for the HTTP handler that
// answers peers.
func (d *Directory) LocalRegistry() *Registry { return d.local }

func (d *Directory) fetch(ctx context.Context, addr, userID string) ([]Session, error) {
	endpoint := fmt.Sprintf("http://%s/internal/presence?user=%s", addr, url.QueryEscape(userID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("presence: %s returned %d", addr, resp.StatusCode)
	}

	var out struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

func (d *Directory) post(ctx context.Context, addr string, s Session) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("http://%s/internal/presence", addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		d.failures.Add(1)
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.failures.Add(1)
		return fmt.Errorf("presence: %s returned %d", addr, resp.StatusCode)
	}
	d.remoteHits.Add(1)
	return nil
}

// Lookup is the presence question the delivery path asks. It exists so fan-out
// and push dispatch depend on the question, not on whether the answer happens
// to be local.
type Lookup interface {
	// Online reports whether a user has any live session.
	Online(ctx context.Context, userID string) bool
	// OfflineUsers filters a recipient list down to those with no session.
	OfflineUsers(ctx context.Context, userIDs []string) []string
}

// LocalLookup answers from one node's registry, for deployments with no
// control plane where every user is local by definition.
type LocalLookup struct{ Registry *Registry }

func (l LocalLookup) Online(_ context.Context, userID string) bool {
	return l.Registry.Online(userID)
}

func (l LocalLookup) OfflineUsers(_ context.Context, userIDs []string) []string {
	return l.Registry.OfflineUsers(userIDs)
}

// OfflineUsers filters a recipient list to those with no live session anywhere.
//
// This is the fan-out hot path, and the naive shape — one lookup per recipient —
// would issue a hundred sequential HTTP calls for a hundred-member group and
// take a hundred round trips to answer. Instead recipients are grouped by shard
// owner and each owner is asked once, concurrently: a hundred-member group
// spread over ten nodes costs ten parallel calls, not a hundred serial ones.
//
// Users whose shard cannot be reached are treated as **online**, matching
// Online: a wrong "offline" here silently drops someone's push notification.
func (d *Directory) OfflineUsers(ctx context.Context, userIDs []string) []string {
	if len(userIDs) == 0 {
		return nil
	}
	if d.resolver == nil {
		d.localHits.Add(1)
		return d.local.OfflineUsers(userIDs)
	}

	// Group by owner, keeping local users out of the network entirely.
	byOwner := map[string][]string{}
	addrOf := map[string]string{}
	var offline []string
	var unreachable int

	for _, u := range userIDs {
		nodeID, addr, local := d.resolver.OwnerOf(u)
		switch {
		case local:
			d.localHits.Add(1)
			if !d.local.Online(u) {
				offline = append(offline, u)
			}
		case nodeID == "" || addr == "":
			// Unowned shard: assume reachable rather than notifying.
			d.unowned.Add(1)
			unreachable++
		default:
			byOwner[nodeID] = append(byOwner[nodeID], u)
			addrOf[nodeID] = addr
		}
	}
	if len(byOwner) == 0 {
		return offline
	}

	type result struct {
		offline []string
		err     error
	}
	results := make(chan result, len(byOwner))
	for nodeID, users := range byOwner {
		go func(addr string, users []string) {
			off, err := d.fetchBatch(ctx, addr, users)
			results <- result{offline: off, err: err}
		}(addrOf[nodeID], users)
	}

	for i := 0; i < len(byOwner); i++ {
		r := <-results
		if r.err != nil {
			// Every user on that owner is assumed reachable.
			d.failures.Add(1)
			continue
		}
		d.remoteHits.Add(1)
		offline = append(offline, r.offline...)
	}
	return offline
}

// fetchBatch asks one owner which of a set of users are offline.
func (d *Directory) fetchBatch(ctx context.Context, addr string, users []string) ([]string, error) {
	body, err := json.Marshal(map[string][]string{"users": users})
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("http://%s/internal/presence/batch", addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("presence: %s returned %d", addr, resp.StatusCode)
	}

	var out struct {
		Offline []string `json:"offline"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Offline, nil
}
