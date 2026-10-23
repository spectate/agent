package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spectate/agent/v2/internal/backoff"
	"github.com/spectate/agent/v2/internal/collector"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/encode"
	"github.com/spectate/agent/v2/internal/metrics"
	pb "github.com/spectate/agent/v2/internal/proto/agentv2"
	"github.com/spectate/agent/v2/internal/spool"
	"github.com/spectate/agent/v2/internal/transport"
)

// counter emits a monotonically increasing value so tests can detect loss,
// duplication and reordering across batches.
type counter struct{ n atomic.Int64 }

func (*counter) Name() string { return "cpu" }

func (c *counter) Collect(_ context.Context, _ time.Time, emit collector.Emit) error {
	emit("test.counter", []metrics.Label{{Key: "k", Value: "v"}}, float64(c.n.Add(1)))
	return nil
}

type received struct {
	req    *pb.IngestRequest
	series []*metrics.Series
}

// fakeAPI is a minimal v2 ingest endpoint.
type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	batches  []received
	failNext int    // respond 503 to this many requests
	status   int    // when non-zero, respond with this status
	resend   bool   // ask for host info once
	config   string // JSON config to return
	srv      *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, config: `{"collection_interval":15,"ingest_interval":60,"collectors":["cpu","agent"]}`}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/agent-api/v2/ingest" || r.Header.Get("Authorization") != "Bearer hst_test" ||
		r.Header.Get("Content-Encoding") != "gzip" {
		http.Error(w, `{"error":{"code":"BAD"}}`, http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		w.Header().Set("Retry-After", "0")
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"error":{"code":"INVALID_PAYLOAD","message":"nope"}}`))
		return
	}
	req, err := encode.Unmarshal(body)
	if err != nil {
		f.t.Errorf("server could not decode batch: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	series, err := encode.Expand(req)
	if err != nil {
		f.t.Errorf("server could not expand batch: %v", err)
	}
	f.batches = append(f.batches, received{req, series})
	resend := f.resend
	f.resend = false
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accepted_samples": 1,
		"duplicate":        false,
		"resend_host_info": resend,
		"config":           json.RawMessage(f.config),
		"next_ingest_in":   60,
	})
}

func (f *fakeAPI) snapshot() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.batches...)
}

func (f *fakeAPI) set(fn func(*fakeAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func newRunner(t *testing.T, api *fakeAPI, sp *spool.Spool, c collector.Collector) *Runner {
	t.Helper()
	if sp == nil {
		var err error
		if sp, err = spool.Open(t.TempDir(), 1<<20, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	return New(Options{
		Client:     transport.New(api.srv.URL, "hst_test"),
		Spool:      sp,
		State:      config.State{HostToken: "hst_test", Remote: config.DefaultRemote()},
		Collectors: []collector.Collector{c},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		HostInfo: func(context.Context) (*pb.HostInfo, error) {
			return &pb.HostInfo{Info: []*pb.Label{{Key: "hostname", Value: "test-01"}}}, nil
		},
		Backoff:         backoff.New(2*time.Millisecond, 10*time.Millisecond),
		DrainPause:      time.Millisecond,
		FirstShipDelay:  20 * time.Millisecond,
		IntervalUnit:    time.Millisecond, // "15s" collection = 15ms, "60s" shipping = 60ms
		HostInfoRefresh: time.Hour,
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// counterValues returns all test.counter values in received order.
func counterValues(batches []received) []float64 {
	var out []float64
	for _, b := range batches {
		for _, s := range b.series {
			if s.Metric == "test.counter" {
				out = append(out, s.Values...)
			}
		}
	}
	return out
}

func assertContiguous(t *testing.T, values []float64) {
	t.Helper()
	for i, v := range values {
		if v != float64(i+1) {
			t.Fatalf("sample %d = %v: samples lost, duplicated or reordered: %v", i, v, values)
		}
	}
}

func TestEndToEndBatchingConfigAndShutdownFlush(t *testing.T) {
	api := newFakeAPI(t)
	api.config = `{"collection_interval":20,"ingest_interval":60,"collectors":["cpu","agent"],"disk":{"exclude_mountpoints":["/boot"]}}`
	c := &counter{}
	r := newRunner(t, api, nil, c)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()

	waitFor(t, "3 batches", func() bool { return len(api.snapshot()) >= 3 })
	cancel()
	<-done

	batches := api.snapshot()
	first := batches[0].req
	if first.ProtocolVersion != 2 || first.BatchSeq != 1 || first.HostInfo == nil || first.HostInfoHash == "" {
		t.Fatalf("first batch envelope = %+v", first)
	}
	for i, b := range batches[1:] {
		if b.req.HostInfo != nil {
			t.Fatalf("batch %d resent unchanged host info", i+2)
		}
		if b.req.BatchSeq != uint64(i+2) || b.req.AgentInstance != first.AgentInstance {
			t.Fatalf("batch %d seq = %d", i+2, b.req.BatchSeq)
		}
	}

	// Several samples per series per batch: one series dictionary entry, many points.
	var multi bool
	for _, b := range batches {
		for _, s := range b.series {
			if s.Metric == "test.counter" && len(s.Values) > 1 {
				multi = true
				if s.Labels[0] != (metrics.Label{Key: "k", Value: "v"}) {
					t.Fatalf("labels = %+v", s.Labels)
				}
			}
		}
	}
	if !multi {
		t.Fatal("expected batches with several samples per series")
	}

	// Health metrics ride along.
	var health bool
	for _, s := range batches[len(batches)-1].series {
		health = health || s.Metric == "agent.send.latency_ms"
	}
	if !health {
		t.Fatal("missing agent health series")
	}

	// Server config applied; the shutdown flush delivered the last window.
	if got := r.Remote(); got.CollectionInterval != 20 || len(got.Disk.ExcludeMountpoints) != 1 {
		t.Fatalf("server config not applied: %+v", got)
	}
	assertContiguous(t, counterValues(batches))
	if int64(len(counterValues(batches))) != c.n.Load() {
		t.Fatalf("shipped %d of %d samples", len(counterValues(batches)), c.n.Load())
	}
}

func TestOutageSpoolsAndReplaysInOrder(t *testing.T) {
	api := newFakeAPI(t)
	api.failNext = 6
	c := &counter{}
	sp, _ := spool.Open(t.TempDir(), 1<<20, time.Hour)
	r := newRunner(t, api, sp, c)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()

	waitFor(t, "spooling", func() bool { return sp.Len() > 0 })
	waitFor(t, "recovery", func() bool {
		return len(api.snapshot()) >= 5 && sp.Len() == 0
	})
	cancel()
	<-done

	batches := api.snapshot()
	for i := 1; i < len(batches); i++ {
		if batches[i].req.BatchSeq <= batches[i-1].req.BatchSeq {
			t.Fatalf("replay out of order: seq %d after %d", batches[i].req.BatchSeq, batches[i-1].req.BatchSeq)
		}
	}
	assertContiguous(t, counterValues(batches))
	if r.sendFailures.Load() < 6 {
		t.Fatalf("failures = %d", r.sendFailures.Load())
	}
}

func TestOfflineShutdownSpoolsToDiskAndNextStartReplays(t *testing.T) {
	api := newFakeAPI(t)
	api.failNext = 1 << 30 // API unreachable for the whole first run
	dir := t.TempDir()
	sp, _ := spool.Open(dir, 1<<20, time.Hour)
	c := &counter{}
	r := newRunner(t, api, sp, c)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	waitFor(t, "spooled batches", func() bool { return sp.Len() >= 2 })
	cancel()
	<-done

	if len(api.snapshot()) != 0 {
		t.Fatal("nothing should have been accepted")
	}
	spooled := sp.Len()

	// "Restart": a new process with the API back up replays the spool first.
	api.set(func(f *fakeAPI) { f.failNext = 0 })
	sp2, _ := spool.Open(dir, 1<<20, time.Hour)
	if sp2.Len() != spooled {
		t.Fatalf("spool did not survive restart: %d != %d", sp2.Len(), spooled)
	}
	r2 := newRunner(t, api, sp2, c)
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { _ = r2.Run(ctx2); close(done2) }()
	waitFor(t, "replay", func() bool { return sp2.Len() == 0 && len(api.snapshot()) > spooled })
	cancel2()
	<-done2

	batches := api.snapshot()
	if batches[0].req.AgentInstance == batches[len(batches)-1].req.AgentInstance {
		t.Fatal("replayed batches keep their original instance id")
	}
	assertContiguous(t, counterValues(batches))
}

func TestRejectedBatchesAreDroppedNotRetried(t *testing.T) {
	api := newFakeAPI(t)
	api.status = http.StatusRequestEntityTooLarge
	sp, _ := spool.Open(t.TempDir(), 1<<20, time.Hour)
	r := newRunner(t, api, sp, &counter{})

	ctx := context.Background()
	r.collectRound(ctx, time.Now())
	r.ship(ctx, false)

	if sp.Len() != 0 || r.droppedBatches.Load() != 1 || r.backingOff() {
		t.Fatalf("413 must drop the batch: spool=%d dropped=%d", sp.Len(), r.droppedBatches.Load())
	}
}

func TestServerCanRequestHostInfo(t *testing.T) {
	api := newFakeAPI(t)
	r := newRunner(t, api, nil, &counter{})
	ctx := context.Background()
	r.refreshHostInfo(ctx)

	r.ship(ctx, false) // includes host info (first batch)
	api.set(func(f *fakeAPI) { f.resend = true })
	r.ship(ctx, false) // without; response asks for it
	r.ship(ctx, false) // includes it again
	r.ship(ctx, false) // without

	got := api.snapshot()
	want := []bool{true, false, true, false}
	for i, b := range got {
		if (b.req.HostInfo != nil) != want[i] {
			t.Fatalf("batch %d host info present = %v, want %v", i+1, b.req.HostInfo != nil, want[i])
		}
	}
}
