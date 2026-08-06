package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/boltq/boltq/internal/cluster"
	"github.com/boltq/boltq/internal/config"
	"github.com/boltq/boltq/internal/dedup"
	"github.com/boltq/boltq/internal/ephemeral"
	"github.com/boltq/boltq/internal/fanout"
	"github.com/boltq/boltq/internal/gateway"
	"github.com/boltq/boltq/internal/identity"
	"github.com/boltq/boltq/internal/membership"
	"github.com/boltq/boltq/internal/outbox"
	"github.com/boltq/boltq/internal/presence"
	"github.com/boltq/boltq/internal/replication"
	"github.com/boltq/boltq/internal/stream"
	"github.com/boltq/boltq/internal/streamctl"
)

// messagingStack owns every component of the chat/streaming subsystem and the
// order in which they must be shut down.
//
// Shutdown order matters and is the reverse of construction: the gateway stops
// accepting sockets first so no new reads start, then the dispatcher drains,
// then cursors are fsynced, and only then is the log closed. Closing the log
// first would make every in-flight read fail with ErrClosed and would lose the
// cursor progress that says where to resume.
type messagingStack struct {
	Log       *stream.Log
	Cursors   *stream.CursorStore
	Presence  *presence.Registry
	Dedup     *dedup.Table
	Signals   *ephemeral.Hub
	Deliverer *fanout.Deliverer
	Policy    *identity.Policy
	Verifier  *identity.Verifier
	Gateway   *gateway.Gateway
	Dispatch  *outbox.Dispatcher
	RepLeader *replication.Leader
	RepFollow *replication.Follower
	// Reconcile is the node's side of the control plane: it turns replicated
	// partition assignments into local promotions and replication sessions. It
	// is nil when clustering is off, in which case replication falls back to
	// the static leader/follower roles below.
	Reconcile *streamctl.Reconciler
	// PresenceDir routes presence lookups to the node owning each user's shard.
	// Nil without a control plane, where every user is local by definition.
	PresenceDir *presence.Directory
	// Forwarder routes writes for partitions this node hosts but does not lead.
	// Nil without a control plane, where there is nowhere to route to.
	Forwarder *streamctl.Forwarder

	httpServer  *http.Server
	stopStream  func()
	stopCursors func()
}

// buildMessaging constructs the messaging stack. It returns nil when the
// streaming subsystem is disabled, so callers can treat "not configured" and
// "not requested" identically.
// metaNode is nil when clustering is disabled. When present, the messaging
// stack takes its partition leadership from the control plane instead of from
// the static replication role in config.
// metaApplier submits control-plane commands. It is the agent, which routes to
// the controller when this node is not it — a partition leader is usually not
// the Raft leader, and its ISR reports must still reach consensus.
func buildMessaging(cfg *config.Config, nodeID string, metaNode cluster.ControlNode, metaApplier streamctl.Applier) (*messagingStack, error) {
	m := &cfg.Messaging
	if !m.Stream.Enabled {
		return nil, nil
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("messaging config: %w", err)
	}

	dir := m.Stream.Dir
	if dir == "" {
		dir = filepath.Join(cfg.Storage.DataDir, "streams")
	}

	st := &messagingStack{}

	// --- Log ---
	logCfg := stream.LogConfig{
		Dir: dir,
		DefaultTopic: stream.TopicConfig{
			Partitions: m.Stream.DefaultPartitions,
			Partition: stream.PartitionConfig{
				SegmentBytes:   m.Stream.SegmentBytes,
				IndexInterval:  m.Stream.IndexInterval,
				RetentionBytes: m.Stream.RetentionBytes,
				RetentionAge:   m.Stream.RetentionAge.D(),
				SyncOnAppend:   m.Stream.SyncOnAppend,
			},
		},
	}
	slog, err := stream.OpenLog(logCfg)
	if err != nil {
		return nil, fmt.Errorf("open stream log: %w", err)
	}
	st.Log = slog
	st.stopStream = slog.StartMaintenance(m.Stream.MaintenanceInterval.Or(30 * time.Second))
	log.Printf("[messaging] stream log open (dir=%s, topics=%d)", dir, len(slog.TopicNames()))

	// --- Cursors ---
	cursors, err := stream.OpenCursorStore(filepath.Join(dir, "_cursors"))
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("open cursor store: %w", err)
	}
	st.Cursors = cursors
	st.stopCursors = cursors.StartAutoFlush(time.Second)
	log.Printf("[messaging] cursor store open (%d cursors)", cursors.Count())

	// --- Presence ---
	st.Presence = presence.New(presence.Config{
		TTL:           m.Presence.TTL.Or(90 * time.Second),
		SweepInterval: m.Presence.SweepInterval.Or(15 * time.Second),
		NodeID:        nodeID,
		Region:        m.Presence.Region,
	})

	// --- Presence directory ---
	//
	// Sharded, not replicated: a user's sessions live on one node and everyone
	// else asks it. Ten million sockets cannot be mirrored on every node, and
	// the shard map is partition leadership of a reserved topic, so presence
	// fails over exactly like message partitions do.
	if metaNode != nil {
		st.PresenceDir = presence.NewDirectory(presence.DirectoryOptions{
			Registry: st.Presence,
			Resolver: streamctl.NewPresenceResolver(metaNode.Metadata(), nodeID),
			APIKey:   cfg.Security.APIKey,
			Timeout:  m.Presence.TTL.Or(time.Second) / 90,
		})
		presence.RegisterMetrics(st.PresenceDir, nodeID)
		log.Printf("[messaging] presence sharded across the cluster (shard topic %q)", presence.ShardTopic)
	}

	// Whichever presence source the rest of the stack asks. Choosing it once
	// here means fan-out and push dispatch cannot disagree about where presence
	// lives.
	var presenceLookup presence.Lookup = presence.LocalLookup{Registry: st.Presence}
	if st.PresenceDir != nil {
		presenceLookup = st.PresenceDir
	}

	// --- Dedup ---
	st.Dedup = dedup.New(dedup.Config{
		TTL:        m.Dedup.TTL.Or(10 * time.Minute),
		MaxEntries: m.Dedup.MaxEntries,
	})

	// --- Ephemeral signals ---
	st.Signals = ephemeral.New(ephemeral.Config{
		MaxPayload:       m.Signals.MaxPayload,
		RatePerSecond:    m.Signals.RatePerSecond,
		Burst:            m.Signals.Burst,
		SubscriberBuffer: m.Signals.SubscriberBuffer,
	})

	// --- Social graph ---
	//
	// Direct conversations resolve their members from the conversation ID, so
	// they work with no external service. Anything else falls through to the
	// application's membership endpoint, and without one, group conversations
	// are simply unavailable rather than silently open to everyone.
	var groupSource interface {
		Members(ctx context.Context, tenant, groupID string) ([]string, error)
		IsMember(ctx context.Context, tenant, userID, groupID string) (bool, error)
	}
	if m.Chat.MembershipURL != "" {
		client, err := membership.New(membership.Options{
			BaseURL:    m.Chat.MembershipURL,
			Timeout:    m.Chat.MembershipTimeout.Or(3 * time.Second),
			AuthHeader: m.Chat.MembershipAuthHeader,
		})
		if err != nil {
			st.Close()
			return nil, err
		}
		groupSource = client
		log.Printf("[messaging] membership endpoint: %s", m.Chat.MembershipURL)
	} else {
		log.Printf("[messaging] no membership endpoint configured — group conversations are unavailable")
	}

	direct := &membership.DirectMembers{Separator: ":", Fallback: groupSource}

	var checker identity.MembershipChecker = direct
	if ttl := m.Identity.MembershipCacheTTL.Or(30 * time.Second); ttl > 0 {
		checker = identity.NewCachedMembership(direct, ttl, 200000)
	}

	// --- Authorisation ---
	st.Policy = identity.NewPolicy(identity.PolicyConfig{
		Rules:          identity.ChatPolicyRules(),
		Membership:     checker,
		AllowAnonymous: m.Identity.AllowAnonymous,
	})

	if m.Identity.Enabled {
		keys := make([]identity.SigningKey, 0, len(m.Identity.Keys))
		for _, k := range m.Identity.Keys {
			secret, err := k.Resolve()
			if err != nil {
				st.Close()
				return nil, err
			}
			keys = append(keys, identity.SigningKey{ID: k.ID, Secret: []byte(secret)})
		}
		v, err := identity.NewVerifier(identity.VerifierConfig{
			Keys:   keys,
			Issuer: m.Identity.Issuer,
			Leeway: m.Identity.Leeway.Or(60 * time.Second),
		})
		if err != nil {
			st.Close()
			return nil, err
		}
		st.Verifier = v
		log.Printf("[messaging] token verification enabled (%d key(s), issuer=%q)", len(keys), m.Identity.Issuer)
	}

	// --- Fan-out ---
	st.Deliverer, err = fanout.New(fanout.Options{
		Log:      slog,
		Members:  direct,
		Presence: presenceLookup,
		Dedup:    st.Dedup,
		Config: fanout.Config{
			FanoutOnWriteLimit:     m.Chat.FanoutOnWriteLimit,
			ConversationPartitions: m.Chat.ConversationPartitions,
			InboxPartitions:        m.Chat.InboxPartitions,
		},
	})
	if err != nil {
		st.Close()
		return nil, err
	}

	// --- Push dispatch ---
	if m.Push.Enabled {
		notifier, err := outbox.NewWebhookNotifier(outbox.WebhookOptions{
			URL:        m.Push.WebhookURL,
			AuthHeader: m.Push.AuthHeader,
			Timeout:    m.Push.Timeout.Or(10 * time.Second),
		})
		if err != nil {
			st.Close()
			return nil, err
		}
		d, err := outbox.New(outbox.Options{
			Log: slog, Cursors: cursors, Presence: presenceLookup, Notifier: notifier,
			Config: outbox.Config{
				MaxAttempts: m.Push.MaxAttempts,
				GraceDelay:  m.Push.GraceDelay.D(),
			},
		})
		if err != nil {
			st.Close()
			return nil, err
		}
		st.Dispatch = d
		d.WatchInboxes(m.Push.ScanInterval.Or(30 * time.Second))
		log.Printf("[messaging] push dispatcher started (webhook=%s)", m.Push.WebhookURL)
	}

	// --- Replication ---
	//
	// Set up before the gateway so that, when quorum acknowledgement is
	// configured, the very first message a client sends already waits for
	// replicas rather than being silently accepted at weaker durability.
	if m.Replication.Enabled {
		secret, err := m.Replication.ResolveSecret()
		if err != nil {
			st.Close()
			return nil, err
		}

		// Controller-driven mode. Every node runs a replication listener and
		// lets the control plane decide which partitions it leads, because
		// leadership is now per-partition and moves: a node is simultaneously
		// leader of some partitions and follower of others, and the static
		// roles below cannot express that.
		if metaNode != nil {
			leader, err := replication.NewLeader(slog, replication.LeaderConfig{
				Addr:         m.Replication.Listen,
				NodeID:       nodeID,
				Secret:       secret,
				MinInSync:    m.Replication.MinInSync,
				AckTimeout:   m.Replication.AckTimeout.Or(5 * time.Second),
				MaxLagInSync: m.Replication.MaxLagInSync,
			})
			if err != nil {
				st.Close()
				return nil, err
			}
			if err := leader.Start(); err != nil {
				st.Close()
				return nil, err
			}
			st.RepLeader = leader

			if m.Replication.MinInSync > 1 {
				slog.SetAckWaiter(leader)
				log.Printf("[messaging] quorum acknowledgement on (min_in_sync=%d)",
					m.Replication.MinInSync)
			}

			st.Reconcile = streamctl.New(slog, metaNode.Metadata(), metaApplier, leader, streamctl.Config{
				NodeID:      nodeID,
				Secret:      secret,
				SyncOnApply: m.Replication.SyncOnApply,
			})
			st.Reconcile.Start()

			// Write routing. Without it, enforcement turns every message that
			// lands on a non-leader into an error the user sees; with it, the
			// write simply travels to the right node.
			st.Forwarder = streamctl.NewForwarder(metaNode.Metadata(), streamctl.ForwarderConfig{
				NodeID: nodeID,
				APIKey: cfg.Security.APIKey,
			})
			slog.SetWriteForwarder(st.Forwarder)
			streamctl.RegisterMetrics(st.Forwarder)

			log.Printf("[messaging] replication follows control-plane assignments (node=%s)", nodeID)
		} else {
			// Static mode. Roles come from config and never change, so a leader
			// failure needs a human. Kept because it is the only thing that
			// works without a control plane, and because it is a legitimate
			// deployment for a two-node hot standby.
			if err := buildStaticReplication(st, slog, m, nodeID, secret); err != nil {
				st.Close()
				return nil, err
			}
		}
	}

	// --- Gateway ---
	if m.Gateway.Enabled {
		g, err := gateway.New(gateway.Options{
			Log: slog, Cursors: cursors, Deliver: st.Deliverer,
			Presence: st.Presence, Directory: st.PresenceDir, Signals: st.Signals,
			Policy: st.Policy, Verifier: st.Verifier,
			APIKey: cfg.Security.APIKey,
			Config: gateway.Config{
				ReadLimit:        m.Gateway.ReadLimit,
				PongTimeout:      m.Gateway.PongTimeout.D(),
				SendBuffer:       m.Gateway.SendBuffer,
				ResumeWindow:     m.Gateway.ResumeWindow.D(),
				MaxSubscriptions: m.Gateway.MaxSubscriptions,
				HistoryLimit:     m.Gateway.HistoryLimit,
				AllowedOrigins:   m.Gateway.AllowedOrigins,
				NodeID:           nodeID,
				Region:           m.Presence.Region,
			},
		})
		if err != nil {
			st.Close()
			return nil, err
		}
		st.Gateway = g
		gateway.RegisterMetrics(g, nodeID)

		path := m.Gateway.Path
		if path == "" {
			path = "/ws"
		}
		if m.Gateway.Port > 0 {
			if err := st.serveGateway(cfg, m, path); err != nil {
				st.Close()
				return nil, err
			}
		} else {
			log.Printf("[messaging] gateway will be mounted on the admin HTTP server at %s", path)
		}
	}

	return st, nil
}

// buildStaticReplication wires the pre-control-plane leader/follower roles.
//
// Cleanup is the caller's job: it already owns st and closes the whole stack on
// any construction failure, so returning a bare error here keeps one teardown
// path instead of two that must agree.
func buildStaticReplication(st *messagingStack, slog *stream.Log, m *config.MessagingConfig, nodeID, secret string) error {
	switch m.Replication.Role {
	case "":
		// Config validation accepts an empty role because it means
		// controller-driven, but we only get here with no control plane. Saying
		// so beats starting a node that replicates nothing and looks healthy.
		return fmt.Errorf("messaging.replication has no role and clustering is disabled: " +
			"set cluster.enabled for controller-driven leadership, or pick a static role")

	case "leader":
		// Open a new leadership term before the listener accepts anything.
		// Records written under the previous term are indistinguishable from
		// this one's unless the epoch moves first, and a follower reconnecting
		// mid-promotion would then have no way to tell which of its records the
		// cluster actually committed.
		epoch := slog.NextLeaderEpoch()
		if err := slog.BecomeLeader(epoch); err != nil {
			return fmt.Errorf("open leader epoch: %w", err)
		}
		log.Printf("[messaging] leading at epoch %d", epoch)

		leader, err := replication.NewLeader(slog, replication.LeaderConfig{
			Addr:         m.Replication.Listen,
			NodeID:       nodeID,
			Secret:       secret,
			MinInSync:    m.Replication.MinInSync,
			AckTimeout:   m.Replication.AckTimeout.Or(5 * time.Second),
			MaxLagInSync: m.Replication.MaxLagInSync,
		})
		if err != nil {
			return err
		}
		if err := leader.Start(); err != nil {
			return err
		}
		st.RepLeader = leader

		if m.Replication.MinInSync > 1 {
			// This is the line that changes what a successful send means: from
			// "on this node's disk" to "on N nodes".
			slog.SetAckWaiter(leader)
			log.Printf("[messaging] quorum acknowledgement on (min_in_sync=%d)",
				m.Replication.MinInSync)
		}

	case "follower":
		var assignments []replication.Assignment
		for _, spec := range m.Replication.Topics {
			topic, count, err := config.ParseTopicSpec(spec)
			if err != nil {
				return err
			}
			for pid := int32(0); pid < count; pid++ {
				assignments = append(assignments, replication.Assignment{
					Topic: topic, Partition: pid, PartitionCount: count,
				})
			}
		}

		follower, err := replication.NewFollower(slog, replication.FollowerConfig{
			LeaderAddr:  m.Replication.LeaderAddr,
			NodeID:      nodeID,
			Secret:      secret,
			Assignments: assignments,
			SyncOnApply: m.Replication.SyncOnApply,
		})
		if err != nil {
			return err
		}
		follower.Start()
		st.RepFollow = follower
		log.Printf("[messaging] replicating %d partition(s) from %s",
			len(assignments), m.Replication.LeaderAddr)
	}
	return nil
}

// serveGateway starts a dedicated listener for end-user traffic.
func (st *messagingStack) serveGateway(cfg *config.Config, m *config.MessagingConfig, path string) error {
	mux := http.NewServeMux()
	mux.Handle(path, st.Gateway)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, m.Gateway.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// A slow or absent handshake must not hold a connection open forever;
		// this is the first line of defence against a connection-exhaustion
		// attack on a public listener.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	st.httpServer = srv

	go func() {
		var err error
		if m.Gateway.TLS.Enabled {
			log.Printf("[messaging] gateway listening on %s%s (TLS)", addr, path)
			err = srv.ListenAndServeTLS(m.Gateway.TLS.CertFile, m.Gateway.TLS.KeyFile)
		} else {
			log.Printf("[messaging] gateway listening on %s%s (plaintext — terminate TLS upstream)", addr, path)
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Printf("[messaging] gateway server stopped: %v", err)
		}
	}()
	return nil
}

// Mount attaches the gateway to an existing mux, used when no dedicated
// gateway port is configured.
func (st *messagingStack) Mount(mux *http.ServeMux, path string) {
	if st == nil || st.Gateway == nil {
		return
	}
	if path == "" {
		path = "/ws"
	}
	mux.Handle(path, st.Gateway)
}

// Close shuts the stack down in dependency order.
func (st *messagingStack) Close() {
	if st == nil {
		return
	}

	if st.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		st.httpServer.Shutdown(ctx)
		cancel()
	}
	if st.Gateway != nil {
		st.Gateway.Close()
	}
	if st.Dispatch != nil {
		st.Dispatch.Close()
	}
	// The reconciler stops before replication: it owns the follower sessions it
	// created, and letting it rebuild one while the stack is tearing down would
	// open a connection nothing will ever close.
	if st.Reconcile != nil {
		st.Reconcile.Close()
	}
	// Replication stops before the log closes: a follower still applying a
	// batch into a closed log would fail every record noisily for no reason.
	if st.RepFollow != nil {
		st.RepFollow.Close()
	}
	if st.RepLeader != nil {
		st.RepLeader.Close()
	}
	if st.Signals != nil {
		st.Signals.Close()
	}
	if st.Dedup != nil {
		st.Dedup.Close()
	}
	if st.Presence != nil {
		st.Presence.Close()
	}
	if st.stopCursors != nil {
		st.stopCursors()
	}
	if st.Cursors != nil {
		// Cursors are fsynced on close: unlike message data, losing them is
		// avoidable at shutdown and costs users redelivered messages.
		if err := st.Cursors.Close(); err != nil {
			log.Printf("[messaging] cursor store close: %v", err)
		}
	}
	if st.stopStream != nil {
		st.stopStream()
	}
	if st.Log != nil {
		if err := st.Log.Close(); err != nil {
			log.Printf("[messaging] stream log close: %v", err)
		}
	}
}
