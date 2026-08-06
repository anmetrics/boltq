package presence

import (
	"github.com/boltq/boltq/internal/metrics"
)

// boltq_presence_unowned_total is the one to alert on.
//
// It counts lookups for a shard whose owner was fenced and not yet reassigned.
// While it climbs, every push decision for the users on that shard is being made
// on a guess, and every fan-out that needed their session list is failing. It is
// the presence equivalent of a partition being offline, and it resolves the same
// way: the controller assigns a new leader.
//
// boltq_presence_remote_total against boltq_presence_local_total tells you
// whether presence traffic is worth what it costs. Almost all remote means the
// shard map and the connection map disagree completely — expected, since the
// load balancer chooses one and a hash chooses the other, but worth knowing
// before blaming the network.

// RegisterMetrics exposes presence routing.
func RegisterMetrics(d *Directory, nodeID string) {
	if d == nil {
		metrics.Unregister("presence")
		return
	}
	labels := map[string]string{"node": nodeID}
	metrics.Register("presence", metrics.CollectorFunc(func() []metrics.Sample {
		s := d.Stats()
		out := []metrics.Sample{
			{
				Name: "boltq_presence_local_total", Type: metrics.TypeCounter,
				Help:   "Presence operations served from a shard this node owns",
				Labels: labels, Value: float64(s.LocalHits),
			},
			{
				Name: "boltq_presence_remote_total", Type: metrics.TypeCounter,
				Help:   "Presence operations that required a call to the shard owner",
				Labels: labels, Value: float64(s.RemoteHits),
			},
			{
				Name: "boltq_presence_failures_total", Type: metrics.TypeCounter,
				Help:   "Presence lookups that could not reach the shard owner",
				Labels: labels, Value: float64(s.Failures),
			},
			{
				Name: "boltq_presence_unowned_total", Type: metrics.TypeCounter,
				Help:   "Lookups for a shard with no current owner; those users' push decisions are guesses",
				Labels: labels, Value: float64(s.Unowned),
			},
		}
		if r := d.LocalRegistry(); r != nil {
			out = append(out, metrics.Sample{
				Name: "boltq_presence_sessions_local", Type: metrics.TypeGauge,
				Help:   "Sessions held in shards this node owns",
				Labels: labels, Value: float64(r.Stats().Sessions),
			})
		}
		return out
	}))
}

// UnregisterMetrics detaches the collector.
func UnregisterMetrics() { metrics.Unregister("presence") }
