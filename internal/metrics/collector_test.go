package metrics

import (
	"strings"
	"testing"
)

func TestCollectorsAppear(t *testing.T) {
	Register("x", CollectorFunc(func() []Sample {
		return []Sample{
			{Name: "boltq_test_gauge", Help: "h", Type: TypeGauge, Labels: map[string]string{"node": `a"b`}, Value: 3},
			{Name: "boltq_test_gauge", Type: TypeGauge, Labels: map[string]string{"node": "c"}, Value: 4},
		}
	}))
	defer Unregister("x")

	out := Global().Prometheus()
	if strings.Count(out, "# TYPE boltq_test_gauge") != 1 {
		t.Errorf("TYPE repeated for one metric name:\n%s", out)
	}
	if !strings.Contains(out, `boltq_test_gauge{node="a\"b"} 3`) {
		t.Errorf("label not escaped:\n%s", out)
	}
}
