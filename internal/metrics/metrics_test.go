package metrics

import (
	"math"
	"testing"
)

func TestAccumulatorGroupsSeriesAndBoundsMemory(t *testing.T) {
	a := NewAccumulator(5)
	disk := []Label{{Key: "mountpoint", Value: "/"}}
	a.Add("m", nil, 1, 1)
	a.Add("m", disk, 1, 2)
	a.Add("m", nil, 2, 3)
	a.Add("m", disk, 2, 4)
	a.Add("nan", nil, 2, math.NaN()) // ignored
	a.Add("m", nil, 3, 5)
	a.Add("m", nil, 4, 6) // over the bound
	a.Add("m", nil, 5, 7) // over the bound

	series := a.Drain()
	if len(series) != 2 || len(series[0].Values) != 3 || len(series[1].Values) != 2 {
		t.Fatalf("series = %+v", series)
	}
	if series[1].Labels[0].Value != "/" || a.Dropped() != 2 || a.Len() != 0 {
		t.Fatalf("labels/dropped/len wrong: %+v %d %d", series[1].Labels, a.Dropped(), a.Len())
	}

	// Callers may reuse their label slice: the accumulator keeps a copy.
	labels := []Label{{Key: "k", Value: "a"}}
	a.Add("x", labels, 1, 1)
	labels[0].Value = "b"
	if got := a.Drain()[0].Labels[0].Value; got != "a" {
		t.Fatalf("label aliasing: %q", got)
	}
}
