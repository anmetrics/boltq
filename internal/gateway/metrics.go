package gateway

import (
	"github.com/boltq/boltq/internal/metrics"
)

// boltq_gateway_attached is the number to autoscale on.
//
// Not CPU. A gateway node's cost is dominated by the connections it holds, not
// by the work it does for them: an idle socket still consumes a file
// descriptor, a read buffer and a write buffer, and ten million idle sockets is
// a capacity problem while the CPU sits near zero. Scaling on CPU would add
// nodes long after the ones running were full.
//
// boltq_gateway_slow_client_drops is the quality signal. It counts connections
// closed because the client stopped reading, which is normal in small numbers —
// phones go through tunnels — and a sign of a saturated node in large ones.

// RegisterMetrics exposes gateway connection state.
func RegisterMetrics(g *Gateway, nodeID string) {
	if g == nil {
		metrics.Unregister("gateway")
		return
	}
	labels := map[string]string{"node": nodeID}
	metrics.Register("gateway", metrics.CollectorFunc(func() []metrics.Sample {
		s := g.Stats()
		return []metrics.Sample{
			{
				Name: "boltq_gateway_attached", Type: metrics.TypeGauge,
				Help:   "Live WebSocket connections on this node — the capacity signal to scale on",
				Labels: labels, Value: float64(s.Attached),
			},
			{
				Name: "boltq_gateway_sessions", Type: metrics.TypeGauge,
				Help:   "Sessions held for resume, including clients currently disconnected",
				Labels: labels, Value: float64(s.Sessions),
			},
			{
				Name: "boltq_gateway_connections_total", Type: metrics.TypeCounter,
				Help:   "Connections accepted since start",
				Labels: labels, Value: float64(s.Connections),
			},
			{
				Name: "boltq_gateway_resumed_total", Type: metrics.TypeCounter,
				Help:   "Connections that resumed an existing session rather than starting fresh",
				Labels: labels, Value: float64(s.Resumed),
			},
			{
				Name: "boltq_gateway_slow_client_drops_total", Type: metrics.TypeCounter,
				Help:   "Connections closed because the client stopped reading",
				Labels: labels, Value: float64(s.SlowClientDrops),
			},
			{
				Name: "boltq_gateway_auth_failures_total", Type: metrics.TypeCounter,
				Help:   "Connections rejected for a bad or missing token",
				Labels: labels, Value: float64(s.AuthFailures),
			},
			{
				Name: "boltq_gateway_records_out_total", Type: metrics.TypeCounter,
				Help:   "Records delivered to clients",
				Labels: labels, Value: float64(s.RecordsOut),
			},
		}
	}))
}

// UnregisterMetrics detaches the collector.
func UnregisterMetrics() { metrics.Unregister("gateway") }
