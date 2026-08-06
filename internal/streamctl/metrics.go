package streamctl

import (
	"github.com/boltq/boltq/internal/metrics"
)

// Forwarding volume is the signal that says whether partition placement matches
// where clients actually land.
//
// A healthy cluster forwards some writes — clients land wherever the load
// balancer sends them, and no placement can match that exactly. A cluster
// forwarding *most* of its writes is paying a network hop on nearly every
// message, and the fix is client-side partition awareness, not more nodes.
//
// boltq_forward_no_leader is the one to alert on. It means writes are failing
// because a partition has no in-sync replica left, which is an outage for the
// users on that partition even though every process is running.

// RegisterMetrics exposes write-routing counters.
func RegisterMetrics(f *Forwarder) {
	if f == nil {
		metrics.Unregister("forwarder")
		return
	}
	metrics.Register("forwarder", metrics.CollectorFunc(func() []metrics.Sample {
		s := f.Stats()
		return []metrics.Sample{
			{
				Name: "boltq_forward_total", Type: metrics.TypeCounter,
				Help:  "Writes routed to the node leading their partition",
				Value: float64(s.Forwarded),
			},
			{
				Name: "boltq_forward_failed", Type: metrics.TypeCounter,
				Help:  "Forwarded writes the peer refused or could not receive",
				Value: float64(s.Failed),
			},
			{
				Name: "boltq_forward_no_leader", Type: metrics.TypeCounter,
				Help:  "Writes dropped because the partition had no leader; an outage for those users",
				Value: float64(s.NoLeader),
			},
		}
	}))
}

// UnregisterMetrics detaches the collector.
func UnregisterMetrics() { metrics.Unregister("forwarder") }
