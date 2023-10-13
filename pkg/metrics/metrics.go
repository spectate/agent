package metrics

import (
	"github.com/spectate/agent/pkg/proto/pb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sync"
)

// Batch allows the collection of multiple data points, and to send it to the Collector after finalization.
type Batch struct {
	data   []*pb.MetricsPayload_Metric
	mux    sync.Mutex
	dataCh chan<- []*pb.MetricsPayload_Metric
}

// NewMetricsBatch creates and returns a new MetricsBatch.
func NewMetricsBatch(dataCh chan<- []*pb.MetricsPayload_Metric) *Batch {
	return &Batch{
		data:   []*pb.MetricsPayload_Metric{},
		mux:    sync.Mutex{},
		dataCh: dataCh,
	}
}

// add accepts a metric key and a value, inserting it into the batch data.
// the key should be formatted by <namespace>.<metric_group>.<metric_name> (e.g. system.cpu.user)
func (s *Batch) add(key string, value interface{}, labels []*pb.MetricsPayload_Label, metricType pb.MetricsPayload_MetricType) {
	s.mux.Lock()
	defer s.mux.Unlock()

	var metricValue *pb.MetricsPayload_Metric

	switch metricType {
	case pb.MetricsPayload_GAUGE:
		metricValue = &pb.MetricsPayload_Metric{
			Key:    key,
			Labels: labels,
			Type:   metricType,
			Value: &pb.MetricsPayload_Metric_GaugeValue{
				GaugeValue: &pb.MetricsPayload_GaugeValue{
					Value: value.(float64),
				},
			},
		}
	case pb.MetricsPayload_INFO:
		metricValue = &pb.MetricsPayload_Metric{
			Key:    key,
			Labels: labels,
			Type:   metricType,
			Value: &pb.MetricsPayload_Metric_InfoValue{
				InfoValue: &pb.MetricsPayload_InfoValue{
					Info: value.([]*pb.MetricsPayload_Label),
				},
			},
		}
	default:
		return
	}

	metricValue.Timestamp = timestamppb.Now()
	s.data = append(s.data, metricValue)
}

// Gauge accepts a metric key and a value, inserting it into the batch data.
func (s *Batch) Gauge(key string, value float64, labels []*pb.MetricsPayload_Label) {
	s.add(key, value, labels, pb.MetricsPayload_GAUGE)
}

// Info accepts a metric key and a value, inserting it into the batch data.
func (s *Batch) Info(key string, value []*pb.MetricsPayload_Label, labels []*pb.MetricsPayload_Label) {
	s.add(key, value, labels, pb.MetricsPayload_INFO)
}

// Commit finalizes the data, adding a timestamp, and then sends it to Collector.
// Data gets grouped by timestamp, and then sent to the API.
func (s *Batch) Commit() {
	s.mux.Lock()
	defer s.mux.Unlock()

	if len(s.data) == 0 {
		// No data was committed, just ignore.
		return
	}

	s.dataCh <- s.data

	// Reset data
	s.data = []*pb.MetricsPayload_Metric{}
}
