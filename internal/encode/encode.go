// Package encode turns a shipping window into a gzip-compressed v2
// IngestRequest (see laravel/proto/spectate/agent/v2/ingest.proto).
package encode

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sync"

	"github.com/spectate/agent/v2/internal/metrics"
	pb "github.com/spectate/agent/v2/internal/proto/agentv2"
	"google.golang.org/protobuf/proto"
)

// ProtocolVersion is the wire protocol implemented by this package.
const ProtocolVersion = 2

// MaxScale is the largest decimal scale tried for the decimal value encoding.
const MaxScale = 6

// maxExactInt is 2^53: integers up to this magnitude are exact as float64.
const maxExactInt = 1 << 53

var pow10 = [...]float64{1, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6}

// Envelope is the per-batch metadata.
type Envelope struct {
	AgentVersion  string
	AgentInstance string
	BatchSeq      uint64
	CreatedAtMs   int64
	HostInfoHash  string
	HostInfo      *pb.HostInfo // nil unless it must be (re)sent
}

// Build assembles the IngestRequest for one window.
func Build(env Envelope, series []*metrics.Series) *pb.IngestRequest {
	req := &pb.IngestRequest{
		ProtocolVersion: ProtocolVersion,
		AgentVersion:    env.AgentVersion,
		AgentInstance:   env.AgentInstance,
		BatchSeq:        env.BatchSeq,
		CreatedAtMs:     env.CreatedAtMs,
		HostInfoHash:    env.HostInfoHash,
		HostInfo:        env.HostInfo,
	}

	strs := newStringTable()
	base := int64(math.MaxInt64)
	for _, s := range series {
		if len(s.Timestamps) > 0 && s.Timestamps[0] < base {
			base = s.Timestamps[0]
		}
	}
	if base == math.MaxInt64 {
		base = 0
	}
	req.BaseTimestampMs = base

	req.Series = make([]*pb.Series, 0, len(series))
	for _, s := range series {
		if len(s.Timestamps) == 0 || len(s.Timestamps) != len(s.Values) {
			continue
		}

		out := &pb.Series{
			Metric:          strs.ref(s.Metric),
			TimestampDeltas: make([]int64, len(s.Timestamps)),
		}
		if len(s.Labels) > 0 {
			out.Labels = make([]uint32, 0, 2*len(s.Labels))
			for _, l := range s.Labels {
				out.Labels = append(out.Labels, strs.ref(l.Key), strs.ref(l.Value))
			}
		}

		prev := base
		for i, ts := range s.Timestamps {
			out.TimestampDeltas[i] = ts - prev
			prev = ts
		}

		if scale, ints, ok := DecimalEncode(s.Values); ok {
			out.Scale = uint32(scale)
			out.ValueDeltas = make([]int64, len(ints))
			var p int64
			for i, v := range ints {
				out.ValueDeltas[i] = v - p
				p = v
			}
		} else {
			out.Values = append([]float64(nil), s.Values...)
		}

		req.Series = append(req.Series, out)
	}

	req.Strings = strs.list
	return req
}

// DecimalEncode finds the smallest scale (0..MaxScale) at which every value is
// exactly float64(int)/10^scale, the operation the server performs when
// decoding. It returns the scaled integers, or ok=false when only the raw
// double encoding is lossless.
func DecimalEncode(values []float64) (scale int, ints []int64, ok bool) {
	ints = make([]int64, len(values))
outer:
	for scale = 0; scale <= MaxScale; scale++ {
		p := pow10[scale]
		for i, v := range values {
			r := math.Round(v * p)
			if math.Abs(r) > maxExactInt || r/p != v || (v == 0 && math.Signbit(v)) {
				continue outer
			}
			ints[i] = int64(r)
		}
		return scale, ints, true
	}
	return 0, nil, false
}

// Marshal serialises and gzip-compresses the request.
func Marshal(req *pb.IngestRequest) ([]byte, error) {
	raw, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal ingest request: %w", err)
	}
	return Gzip(raw)
}

var gzipPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(nil, gzip.BestCompression)
	return w
}}

// Gzip compresses b.
func Gzip(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(b)/3 + 64)

	w := gzipPool.Get().(*gzip.Writer)
	defer gzipPool.Put(w)
	w.Reset(&buf)

	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// HostInfoHash is the hex sha256 of the deterministic encoding of info.
func HostInfoHash(info *pb.HostInfo) string {
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(info)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type stringTable struct {
	index map[string]uint32
	list  []string
}

func newStringTable() *stringTable {
	return &stringTable{index: map[string]uint32{"": 0}, list: []string{""}}
}

func (t *stringTable) ref(s string) uint32 {
	if i, ok := t.index[s]; ok {
		return i
	}
	i := uint32(len(t.list))
	t.index[s] = i
	t.list = append(t.list, s)
	return i
}
