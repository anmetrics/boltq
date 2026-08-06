package cluster

import (
	"github.com/boltq/boltq/internal/metrics"
)

// The numbers that matter when a cluster is large are not throughput counters.
// They are the ones that answer "is anything broken right now", and there are
// exactly three worth waking someone for:
//
//	partitions_offline           writes to these fail; data is not lost, but
//	                             some users cannot send
//	partitions_under_replicated  one more failure loses acknowledged records
//	brokers_fenced               nodes the controller has stopped hearing from
//
// Everything else here is for capacity planning and for seeing whether the
// rebalancer is doing what you asked.

// RegisterMetrics exposes control-plane state to the metrics endpoint.
//
// Collected on scrape rather than pushed, because these are states, not events:
// "how many partitions are offline" has an answer at every instant, and no
// counter can produce it.
func RegisterMetrics(meta *MetadataStore, nodeID string) {
	metrics.Register("cluster", metrics.CollectorFunc(func() []metrics.Sample {
		return collectClusterSamples(meta, nodeID)
	}))
}

// UnregisterMetrics detaches the collector.
func UnregisterMetrics() { metrics.Unregister("cluster") }

func collectClusterSamples(meta *MetadataStore, nodeID string) []metrics.Sample {
	if meta == nil {
		return nil
	}

	assignments := meta.Assignments()
	brokers := meta.Brokers()

	var offline, underReplicated, ledHere, atRisk int
	leaderCount := map[string]int{}

	for _, a := range assignments {
		if a.Leader == "" {
			offline++
		} else {
			leaderCount[a.Leader]++
			if a.Leader == nodeID {
				ledHere++
			}
		}
		if len(a.ISR) < len(a.Replicas) {
			underReplicated++
		}
		// One in-sync replica means the next failure takes the partition
		// offline and loses whatever the leader had not replicated. It is a
		// distinct and more urgent state than merely under-replicated.
		if len(a.ISR) <= 1 {
			atRisk++
		}
	}

	fenced := 0
	for _, b := range brokers {
		if b.Fenced {
			fenced++
		}
	}

	out := []metrics.Sample{
		{
			Name: "boltq_partitions_total", Type: metrics.TypeGauge,
			Help:  "Partitions known to the control plane",
			Value: float64(len(assignments)),
		},
		{
			Name: "boltq_partitions_offline", Type: metrics.TypeGauge,
			Help:  "Partitions with no leader; writes to these fail",
			Value: float64(offline),
		},
		{
			Name: "boltq_partitions_under_replicated", Type: metrics.TypeGauge,
			Help:  "Partitions whose in-sync set is smaller than their replica set",
			Value: float64(underReplicated),
		},
		{
			Name: "boltq_partitions_at_risk", Type: metrics.TypeGauge,
			Help:  "Partitions down to a single in-sync replica; one more failure loses acknowledged records",
			Value: float64(atRisk),
		},
		{
			Name: "boltq_brokers_total", Type: metrics.TypeGauge,
			Help:  "Brokers registered with the control plane",
			Value: float64(len(brokers)),
		},
		{
			Name: "boltq_brokers_fenced", Type: metrics.TypeGauge,
			Help:  "Brokers the controller has stopped hearing from",
			Value: float64(fenced),
		},
		{
			Name: "boltq_partitions_led_local", Type: metrics.TypeGauge,
			Help:  "Partitions this node currently leads",
			Value: float64(ledHere),
		},
		{
			Name: "boltq_metadata_version", Type: metrics.TypeCounter,
			Help:  "Control-plane metadata version; stalling here means this node stopped receiving updates",
			Value: float64(meta.Version()),
		},
	}

	// Per-node leadership, which is how leadership imbalance becomes visible.
	// A node leading three times its share is a hot spot the rebalancer has not
	// corrected — or cannot, because the placement does not allow it.
	for _, b := range brokers {
		out = append(out, metrics.Sample{
			Name: "boltq_partitions_led", Type: metrics.TypeGauge,
			Help:   "Partitions led, by node",
			Labels: map[string]string{"node": b.NodeID},
			Value:  float64(leaderCount[b.NodeID]),
		})
	}
	return out
}
