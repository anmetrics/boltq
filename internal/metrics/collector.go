package metrics

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Metrics that matter in a large cluster are not counters the server increments
// as it works — they are *states* it has to be asked about: how many partitions
// have no leader right now, how many are under-replicated, how leadership is
// spread across nodes.
//
// A counter cannot express those. So instead of every subsystem pushing numbers
// into a shared struct, subsystems register a collector and are asked at scrape
// time. That also keeps the dependency pointing the right way: the metrics
// package knows nothing about partitions, gateways or consensus.

// SampleType is the Prometheus metric type.
type SampleType string

const (
	// TypeCounter only ever increases; a decrease means a restart.
	TypeCounter SampleType = "counter"
	// TypeGauge can move in both directions — the shape most cluster health
	// signals take.
	TypeGauge SampleType = "gauge"
)

// Sample is one exported measurement.
type Sample struct {
	Name   string
	Help   string
	Type   SampleType
	Labels map[string]string
	Value  float64
}

// Collector produces samples on demand.
//
// Collect is called on every scrape, so it must be cheap and must not block on
// anything slow — a collector that waits on consensus would make the metrics
// endpoint fail exactly when the cluster is unhealthy and the endpoint matters
// most.
type Collector interface {
	Collect() []Sample
}

// CollectorFunc adapts a function to Collector.
type CollectorFunc func() []Sample

func (f CollectorFunc) Collect() []Sample { return f() }

var (
	collectorMu sync.RWMutex
	collectors  = map[string]Collector{}
)

// Register adds or replaces a named collector. Naming them means a component
// that is rebuilt — a reconciler restarted, a gateway replaced — does not leave
// a stale collector reporting numbers from a dead object.
func Register(name string, c Collector) {
	collectorMu.Lock()
	defer collectorMu.Unlock()
	if c == nil {
		delete(collectors, name)
		return
	}
	collectors[name] = c
}

// Unregister removes a collector.
func Unregister(name string) { Register(name, nil) }

// collectAll gathers every registered collector's samples.
func collectAll() []Sample {
	collectorMu.RLock()
	names := make([]string, 0, len(collectors))
	for name := range collectors {
		names = append(names, name)
	}
	sort.Strings(names)
	list := make([]Collector, 0, len(names))
	for _, name := range names {
		list = append(list, collectors[name])
	}
	collectorMu.RUnlock()

	var out []Sample
	for _, c := range list {
		out = append(out, c.Collect()...)
	}
	return out
}

// renderSamples formats samples as Prometheus text.
//
// HELP and TYPE are emitted once per metric name even when several samples
// share it under different labels — repeating them is a protocol violation that
// some scrapers accept and others reject outright.
func renderSamples(samples []Sample) string {
	if len(samples) == 0 {
		return ""
	}

	// Stable output: a metrics endpoint whose line order changes between
	// scrapes is miserable to diff when something is wrong.
	sort.SliceStable(samples, func(i, j int) bool {
		if samples[i].Name != samples[j].Name {
			return samples[i].Name < samples[j].Name
		}
		return labelString(samples[i].Labels) < labelString(samples[j].Labels)
	})

	var b strings.Builder
	seen := map[string]bool{}
	for _, s := range samples {
		if !seen[s.Name] {
			seen[s.Name] = true
			if s.Help != "" {
				b.WriteString("# HELP " + s.Name + " " + s.Help + "\n")
			}
			typ := s.Type
			if typ == "" {
				typ = TypeGauge
			}
			b.WriteString("# TYPE " + s.Name + " " + string(typ) + "\n")
		}
		b.WriteString(s.Name)
		b.WriteString(labelString(s.Labels))
		b.WriteString(" ")
		b.WriteString(strconv.FormatFloat(s.Value, 'g', -1, 64))
		b.WriteString("\n")
	}
	return b.String()
}

// labelString renders labels in a stable order.
func labelString(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(labels[k]))
		b.WriteString(`"`)
	}
	b.WriteString("}")
	return b.String()
}

// escapeLabel escapes the three characters the exposition format reserves.
// Node IDs come from hostnames and topic names come from users, so neither is
// safe to interpolate raw.
func escapeLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}
