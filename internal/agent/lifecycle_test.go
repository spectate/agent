package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spectate/agent/v2/internal/backoff"
	"github.com/spectate/agent/v2/internal/collector"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/metrics"
	pb "github.com/spectate/agent/v2/internal/proto/agentv2"
	"github.com/spectate/agent/v2/internal/spool"
	"github.com/spectate/agent/v2/internal/transport"
)

// logBuffer is a concurrency-safe slog sink.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// count returns how many log lines contain all of parts.
func (b *logBuffer) count(parts ...string) int {
	n := 0
	for line := range strings.SplitSeq(b.String(), "\n") {
		all := line != ""
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			n++
		}
	}
	return n
}

func logTo(r *Runner, level slog.Level) *logBuffer {
	b := &logBuffer{}
	r.log = slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: level}))
	return b
}

const hostDeletedBody = `{"error":{"code":"HOST_DELETED","message":"This host was deleted."}}`

func TestHostDeletedStopsShippingClearsSpoolAndPersistsMarker(t *testing.T) {
	api := newFakeAPI(t)
	api.status = http.StatusGone
	api.errBody = hostDeletedBody
	stateDir := t.TempDir()
	sp, _ := spool.Open(t.TempDir(), 1<<20, time.Hour)
	for i := range 3 { // left over from an outage
		_ = sp.Push([]byte("old"), time.Now().Add(time.Duration(i)*time.Millisecond))
	}
	st := config.State{APIURL: api.srv.URL, HostToken: "hst_test", HostUUID: "u-1", Remote: config.DefaultRemote()}
	if err := config.SaveState(stateDir, st); err != nil {
		t.Fatal(err)
	}
	c := &counter{}
	r := newRunner(t, api, sp, c)
	r.opts.StateDir = stateDir
	r.opts.State = st
	r.state = st
	logs := logTo(r, slog.LevelDebug)

	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrHostDeleted) {
			t.Fatalf("Run = %v, want ErrHostDeleted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner kept going after 410 HOST_DELETED")
	}

	requests := api.count()
	if requests != 1 {
		t.Fatalf("requests = %d: the first 410 must stop shipping (no replay of the remaining spool, no final flush)", requests)
	}
	if sp.Len() != 0 {
		t.Fatalf("spool not cleared: %d batches", sp.Len())
	}
	if n := logs.count("level=ERROR", HostDeletedMessage); n != 1 {
		t.Fatalf("want exactly one ERROR %q, got %d:\n%s", HostDeletedMessage, n, logs)
	}
	saved, err := config.LoadState(stateDir)
	if err != nil || !saved.HostDeleted || saved.HostToken != "hst_test" {
		t.Fatalf("marker not persisted: %+v, %v", saved, err)
	}
	collected := c.n.Load()
	time.Sleep(50 * time.Millisecond)
	if c.n.Load() != collected {
		t.Fatal("collection continued after the host was deleted")
	}

	// Restart: the marker stops the agent before it contacts the API.
	r2 := newRunner(t, api, sp, &counter{})
	r2.opts.State = saved
	logs2 := logTo(r2, slog.LevelDebug)
	if err := r2.Run(context.Background()); !errors.Is(err, ErrHostDeleted) {
		t.Fatalf("restart with marker: %v", err)
	}
	if api.count() != requests {
		t.Fatal("restart with the host_deleted marker must not call the API")
	}
	if logs2.count("level=ERROR", HostDeletedMessage) != 1 {
		t.Fatalf("restart must log the message once:\n%s", logs2)
	}
}

func TestOther410IsRetried(t *testing.T) {
	api := newFakeAPI(t)
	api.status = http.StatusGone
	api.errBody = `{"message":"gone"}`
	sp, _ := spool.Open(t.TempDir(), 1<<20, time.Hour)
	r := newRunner(t, api, sp, &counter{})
	r.ship(context.Background(), false)
	if r.deleted.Load() || sp.Len() != 1 {
		t.Fatalf("a 410 without HOST_DELETED is a transient error: deleted=%v spool=%d", r.deleted.Load(), sp.Len())
	}
}

func TestRevokedTokenSpoolsAndBacksOffLong(t *testing.T) {
	api := newFakeAPI(t)
	api.status = http.StatusUnauthorized
	api.errBody = `{"error":{"code":"INVALID_HOST_TOKEN","message":"Invalid host token."}}`
	sp, _ := spool.Open(t.TempDir(), 1<<20, time.Hour)
	r := newRunner(t, api, sp, &counter{})
	r.opts.AuthRetry = time.Hour
	logs := logTo(r, slog.LevelDebug)
	ctx := context.Background()

	r.collectRound(ctx, time.Now())
	r.ship(ctx, false)
	if sp.Len() != 1 {
		t.Fatalf("401 must keep the batch: spool = %d", sp.Len())
	}
	r.mu.Lock()
	wait := time.Until(r.retryAt)
	r.mu.Unlock()
	if wait < 44*time.Minute || wait > time.Hour {
		t.Fatalf("401 retry in %v, want ~1h (not the normal 5m cap)", wait)
	}

	// While backing off, new windows are spooled without contacting the API.
	r.ship(ctx, false)
	if requests := api.count(); sp.Len() != 2 || requests != 1 {
		t.Fatalf("spool = %d, requests = %d", sp.Len(), requests)
	}

	// The next retry fails again: no second ERROR within the hour.
	r.drain(ctx)
	if n := logs.count("level=ERROR"); n != 1 {
		t.Fatalf("ERROR lines = %d, want 1:\n%s", n, logs)
	}
	if r.deleted.Load() {
		t.Fatal("401 must not be treated as a deleted host")
	}

	// An hour later it is reported again.
	r.limits.now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	r.drain(ctx)
	if n := logs.count("level=ERROR"); n != 2 {
		t.Fatalf("ERROR lines = %d, want 2 after an hour", n)
	}
}

func TestRevokedTokenHonoursLongerRetryAfter(t *testing.T) {
	r := newRunner(t, newFakeAPI(t), nil, &counter{})
	r.opts.AuthRetry = time.Hour
	r.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	r.failed(&transport.Error{Status: 401, RetryAfter: 3 * time.Hour})
	r.mu.Lock()
	defer r.mu.Unlock()
	if wait := time.Until(r.retryAt); wait < 179*time.Minute {
		t.Fatalf("retry in %v, want Retry-After (3h)", wait)
	}
}

func TestClockOffsetWarningIsRateLimited(t *testing.T) {
	api := newFakeAPI(t)
	api.extra = map[string]any{"clock_offset_ms": 95_000}
	r := newRunner(t, api, nil, &counter{})
	logs := logTo(r, slog.LevelInfo)
	now := time.Now()
	r.limits.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		r.ship(ctx, false)
	}
	msg := "host clock is off by 95s; samples are corrected by the server, but fix NTP"
	if n := logs.count("level=WARN", msg); n != 1 {
		t.Fatalf("clock warnings = %d, want 1:\n%s", n, logs)
	}

	now = now.Add(61 * time.Minute)
	r.ship(ctx, false)
	if n := logs.count("level=WARN", msg); n != 2 {
		t.Fatalf("clock warnings after an hour = %d, want 2", n)
	}

	// Within the threshold (either direction): silent.
	now = now.Add(2 * time.Hour)
	api.set(func(f *fakeAPI) { f.extra = map[string]any{"clock_offset_ms": -30_000} })
	r.ship(ctx, false)
	if logs.count("host clock") != 2 {
		t.Fatal("an offset of 30s must not warn")
	}
	api.set(func(f *fakeAPI) { f.extra = map[string]any{"clock_offset_ms": -45_500} })
	r.ship(ctx, false)
	if logs.count("level=WARN", "host clock is off by 45s", "host_clock=ahead") != 1 {
		t.Fatalf("negative offsets warn too:\n%s", logs)
	}
}

func TestAgentUpdateIsPersistedAndLoggedDaily(t *testing.T) {
	api := newFakeAPI(t)
	api.extra = map[string]any{"agent_update": map[string]string{
		"latest_version": "99.1.0", "release_url": "https://github.com/spectate/agent/releases/tag/v99.1.0",
	}}
	stateDir := t.TempDir()
	r := newRunner(t, api, nil, &counter{})
	r.opts.StateDir = stateDir
	logs := logTo(r, slog.LevelInfo)
	now := time.Now()
	r.limits.now = func() time.Time { return now }
	ctx := context.Background()

	r.ship(ctx, false)
	r.ship(ctx, false)
	msg := "spectate-agent 99.1.0 is available: sudo spectate-agent update"
	if n := logs.count("level=INFO", msg); n != 1 {
		t.Fatalf("update hints = %d, want 1:\n%s", n, logs)
	}
	st, err := config.LoadState(stateDir)
	if err != nil || st.AvailableUpdate == nil || st.AvailableUpdate.Version != "99.1.0" {
		t.Fatalf("available update not persisted: %+v, %v", st.AvailableUpdate, err)
	}
	now = now.Add(25 * time.Hour)
	r.ship(ctx, false)
	if logs.count(msg) != 2 {
		t.Fatal("the hint repeats daily")
	}

	// Older / equal / absent announcements clear it.
	for _, extra := range []map[string]any{
		{"agent_update": map[string]string{"latest_version": "1.0.0"}},
		{"agent_update": nil},
	} {
		api.set(func(f *fakeAPI) { f.extra = extra })
		r.ship(ctx, false)
		if st, _ := config.LoadState(stateDir); st.AvailableUpdate != nil {
			t.Fatalf("stale update hint kept after %v", extra)
		}
	}
}

// blocker is a collector stuck in an uninterruptible call until released.
type blocker struct {
	name    string
	release chan struct{}
	starts  atomic.Int32
}

func (b *blocker) Name() string { return b.name }

func (b *blocker) Collect(_ context.Context, _ time.Time, emit collector.Emit) error {
	b.starts.Add(1)
	emit("test.blocker.partial", nil, 1) // emitted before the deadline: kept
	<-b.release                          // ignores ctx, like statfs on a dead NFS mount
	emit("test.blocker.late", nil, 1)    // after the deadline: discarded
	return nil
}

func TestStuckCollectorDoesNotStallOthersOrPileUp(t *testing.T) {
	api := newFakeAPI(t)
	stuck := &blocker{name: "disk", release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-stuck.release:
		default:
			close(stuck.release)
		}
	})
	c := &counter{}
	sp, _ := spool.Open(t.TempDir(), 1<<20, time.Hour)
	r := New(Options{
		Client:     transport.New(api.srv.URL, "hst_test"),
		Spool:      sp,
		State:      config.State{HostToken: "hst_test", Remote: config.DefaultRemote()},
		Collectors: []collector.Collector{stuck, c},
		HostInfo: func(context.Context) (*pb.HostInfo, error) {
			return &pb.HostInfo{}, nil
		},
		Backoff:      backoff.New(time.Millisecond, time.Millisecond),
		IntervalUnit: 100 * time.Millisecond, // 15 "s" interval: round deadline 750ms, clamped to 1s
	})
	logs := logTo(r, slog.LevelInfo)
	ctx := context.Background()

	start := time.Now()
	r.collectRound(ctx, time.Now())
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("round took %v: the stuck collector blocked it", took)
	}
	r.collectRound(ctx, time.Now())
	r.collectRound(ctx, time.Now())

	if got := stuck.starts.Load(); got != 1 {
		t.Fatalf("stuck collector started %d times, want 1 (no goroutine pile-up)", got)
	}
	if got := c.n.Load(); got != 3 {
		t.Fatalf("healthy collector ran %d times, want 3", got)
	}
	if n := logs.count("level=WARN", "collector=disk"); n != 1 {
		t.Fatalf("stuck collector warnings = %d, want 1:\n%s", n, logs)
	}
	if got := r.Stuck(); len(got) != 1 || got[0] != "disk" {
		t.Fatalf("Stuck() = %v", got)
	}

	// Release it: the late sample is discarded and the next round runs it again.
	close(stuck.release)
	waitFor(t, "stuck collector to return", func() bool { return len(r.Stuck()) == 0 })
	r.collectRound(ctx, time.Now())
	if got := stuck.starts.Load(); got != 2 {
		t.Fatalf("recovered collector started %d times, want 2", got)
	}

	counts := map[string]int{}
	for _, s := range r.acc.Drain() {
		counts[s.Metric] += len(s.Values)
	}
	if counts["test.counter"] != 4 || counts["test.blocker.partial"] != 2 || counts["test.blocker.late"] != 1 {
		// late: only the second (unblocked) run's emit, the first run's came after its round closed
		t.Fatalf("samples = %v", counts)
	}
	if logs.count("stuck collector returned") != 1 {
		t.Fatalf("recovery not logged:\n%s", logs)
	}
}

// Samples carry the round's timestamp regardless of which goroutine emitted them.
func TestConcurrentCollectorsShareRoundTimestamp(t *testing.T) {
	r := newRunner(t, newFakeAPI(t), nil, &counter{})
	r.opts.Collectors = append(r.opts.Collectors, &emitter{name: "memory"}, &emitter{name: "load"})
	at := time.UnixMilli(1_700_000_000_000)
	r.collectRound(context.Background(), at)
	n := 0
	for _, s := range r.acc.Drain() {
		for _, ts := range s.Timestamps {
			n++
			if ts != at.UnixMilli() {
				t.Fatalf("%s ts = %d", s.Metric, ts)
			}
		}
	}
	if n != 3 {
		t.Fatalf("samples = %d, want 3", n)
	}
}

type emitter struct{ name string }

func (e *emitter) Name() string { return e.name }

func (e *emitter) Collect(_ context.Context, _ time.Time, emit collector.Emit) error {
	emit("test."+e.name, []metrics.Label{{Key: "k", Value: e.name}}, 1)
	return nil
}

type periodic struct {
	runs atomic.Int64
}

func (*periodic) Name() string { return "services" }

func (*periodic) IntervalSeconds() int { return 60 }

func (p *periodic) Collect(_ context.Context, _ time.Time, emit collector.Emit) error {
	p.runs.Add(1)
	emit("system.services.failed", nil, 0)
	return nil
}

func TestPeriodicCollectorRunsOncePerInterval(t *testing.T) {
	c := &periodic{}
	r := newRunner(t, newFakeAPI(t), nil, c) // "15s" rounds = 15ms, interval "60s" = 60ms
	t0 := time.Now().Truncate(time.Second)
	for i := range 9 { // rounds at 0..120 "s"
		r.collectRound(context.Background(), t0.Add(time.Duration(i)*15*time.Millisecond))
	}
	if got := c.runs.Load(); got != 3 {
		t.Fatalf("ran %d times over 120s, want 3 (at 0, 60 and 120s)", got)
	}
}
