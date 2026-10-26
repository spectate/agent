package encode

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/spectate/agent/v2/internal/metrics"
	pb "github.com/spectate/agent/v2/internal/proto/agentv2"
	"google.golang.org/protobuf/proto"
)

// Unmarshal decompresses and parses a gzip-compressed IngestRequest. It is the
// reference decoder used by tests; the server implements the same rules in PHP.
func Unmarshal(body []byte) (*pb.IngestRequest, error) {
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	req := &pb.IngestRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		return nil, err
	}
	return req, nil
}

// Expand reverses Build: it resolves the string table and delta encodings.
func Expand(req *pb.IngestRequest) ([]*metrics.Series, error) {
	str := func(i uint32) (string, error) {
		if int(i) >= len(req.Strings) {
			if i == 0 {
				return "", nil
			}
			return "", fmt.Errorf("unknown string %d", i)
		}
		return req.Strings[i], nil
	}

	out := make([]*metrics.Series, 0, len(req.Series))
	for n, s := range req.Series {
		name, err := str(s.Metric)
		if err != nil {
			return nil, fmt.Errorf("series[%d]: %w", n, err)
		}
		if len(s.Labels)%2 != 0 {
			return nil, fmt.Errorf("series[%d]: odd label refs", n)
		}
		series := &metrics.Series{Metric: name}
		for i := 0; i < len(s.Labels); i += 2 {
			k, err := str(s.Labels[i])
			if err != nil {
				return nil, err
			}
			v, err := str(s.Labels[i+1])
			if err != nil {
				return nil, err
			}
			series.Labels = append(series.Labels, metrics.Label{Key: k, Value: v})
		}

		ts := req.BaseTimestampMs
		for _, d := range s.TimestampDeltas {
			ts += d
			series.Timestamps = append(series.Timestamps, ts)
		}

		switch {
		case len(s.Values) > 0:
			series.Values = append(series.Values, s.Values...)
		default:
			if s.Scale > MaxScale+9 {
				return nil, fmt.Errorf("series[%d]: scale %d", n, s.Scale)
			}
			p := 1.0
			for range s.Scale {
				p *= 10
			}
			var sum int64
			for _, d := range s.ValueDeltas {
				sum += d
				series.Values = append(series.Values, float64(sum)/p)
			}
		}
		if len(series.Values) != len(series.Timestamps) {
			return nil, fmt.Errorf("series[%d]: %d values for %d timestamps", n, len(series.Values), len(series.Timestamps))
		}
		out = append(out, series)
	}
	return out, nil
}
