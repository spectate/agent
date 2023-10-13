package metrics

//
//import (
//	"sync"
//	"time"
//)
//
//// MetricsBatch allows the collection of multiple data points, and to send it to the Collector after finalization.
//type MetricsBatch struct {
//	data   map[string]float64
//	mux    sync.Mutex
//	dataCh chan<- Payload
//}
//
//// NewMetricsBatch creates and returns a new MetricsBatch.
//func NewMetricsBatch(dataCh chan<- Payload) *MetricsBatch {
//	return &MetricsBatch{
//		data:   make(map[string]float64),
//		mux:    sync.Mutex{},
//		dataCh: dataCh,
//	}
//}
//
//// add accepts a metric key and a value, inserting it into the batch data.
//// the key should be formatted by <namespace>.<metric_group>.<metric_name> (e.g. system.cpu.user)
//func (s *MetricsBatch) add(key string, value float64) {
//	s.mux.Lock()
//	defer s.mux.Unlock()
//
//	s.data[key] = value
//}
//
//// Gauge accepts a metric key and a value, inserting it into the batch data.
//func (s *MetricsBatch) Gauge(key string, value float64) {
//	s.add(key, value)
//}
//
//// Payload is used to accumulate all metric data with their timestamp.
//type Payload struct {
//	Data      map[string]float64 `json:"data"`
//	Timestamp int64              `json:"timestamp"`
//}
//
//// Commit finalizes the data, adding a timestamp, and then sends it to Collector.
//// Data gets grouped by timestamp, and then sent to the API.
//func (s *MetricsBatch) Commit() {
//	s.mux.Lock()
//	defer s.mux.Unlock()
//
//	if len(s.data) == 0 {
//		// No data was committed, just ignore.
//		return
//	}
//
//	payload := Payload{
//		Data:      s.data,
//		Timestamp: time.Now().UnixMilli(),
//	}
//	s.data = make(map[string]float64)
//
//	s.dataCh <- payload
//}
