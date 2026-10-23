// Package metrics holds collected samples between shipping windows.
//
// Samples are grouped by series (metric name + ordered label set) as they are
// added, so a shipping window maps 1:1 onto the columnar v2 payload.
package metrics

import (
	"math"
	"strings"
	"sync"
)

// Label is one key/value pair. Label order is significant: it is preserved on
// the wire and becomes label_names/label_values server-side.
type Label struct {
	Key   string
	Value string
}

// Series is one metric + label set with its samples in insertion order.
type Series struct {
	Metric     string
	Labels     []Label
	Timestamps []int64 // unix ms
	Values     []float64
}

// Accumulator collects samples for the current shipping window. It is safe
// for concurrent use.
type Accumulator struct {
	mu        sync.Mutex
	maxPoints int
	points    int
	index     map[string]int
	series    []*Series
	dropped   uint64
	keyBuf    strings.Builder
}

// NewAccumulator returns an accumulator that holds at most maxPoints samples
// per window; further samples are counted as dropped.
func NewAccumulator(maxPoints int) *Accumulator {
	return &Accumulator{maxPoints: maxPoints, index: make(map[string]int)}
}

// Add records one sample. Non-finite values are ignored.
func (a *Accumulator) Add(metric string, labels []Label, tsMs int64, value float64) {
	if metric == "" || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.points >= a.maxPoints {
		a.dropped++
		return
	}

	a.keyBuf.Reset()
	a.keyBuf.WriteString(metric)
	for _, l := range labels {
		a.keyBuf.WriteByte(0)
		a.keyBuf.WriteString(l.Key)
		a.keyBuf.WriteByte(0)
		a.keyBuf.WriteString(l.Value)
	}
	key := a.keyBuf.String()

	i, ok := a.index[key]
	if !ok {
		i = len(a.series)
		a.index[key] = i
		a.series = append(a.series, &Series{Metric: metric, Labels: append([]Label(nil), labels...)})
	}

	s := a.series[i]
	s.Timestamps = append(s.Timestamps, tsMs)
	s.Values = append(s.Values, value)
	a.points++
}

// Drain returns the current window's series and starts a new window.
func (a *Accumulator) Drain() []*Series {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := a.series
	a.series = nil
	a.index = make(map[string]int, len(out))
	a.points = 0

	return out
}

// Len returns the number of samples in the current window.
func (a *Accumulator) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.points
}

// Dropped returns the total number of samples dropped because the window was full.
func (a *Accumulator) Dropped() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.dropped
}
