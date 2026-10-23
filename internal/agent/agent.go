// Package agent wires collection, batching, spooling and shipping.
//
//	collect loop ── every collection_interval (aligned to the wall clock) ──► Accumulator
//	ship loop    ── every next_ingest_in ──► encode + gzip ──► send ──(fail)──► spool
//	                                                      └── spool non-empty ──► replay oldest first
//
// Failed sends back off exponentially with jitter (honouring Retry-After);
// batches the server rejects as malformed or too large are dropped. On
// shutdown the current window is flushed (or spooled when offline).
package agent

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spectate/agent/v2/internal/backoff"
	"github.com/spectate/agent/v2/internal/collector"
	"github.com/spectate/agent/v2/internal/config"
	"github.com/spectate/agent/v2/internal/encode"
	"github.com/spectate/agent/v2/internal/metrics"
	pb "github.com/spectate/agent/v2/internal/proto/agentv2"
	"github.com/spectate/agent/v2/internal/spool"
	"github.com/spectate/agent/v2/internal/transport"
	"github.com/spectate/agent/v2/internal/version"
)

// maxWindowSamples bounds memory per shipping window (~50 bytes per sample).
const maxWindowSamples = 50_000

// Options configures a Runner. Zero values select production defaults.
type Options struct {
	Client     *transport.Client
	Spool      *spool.Spool
	State      config.State
	StateDir   string // persist server config here ("" disables)
	Collectors []collector.Collector
	Logger     *slog.Logger

	// HostInfo gathers host identity (default collector.HostInfo).
	HostInfo func(ctx context.Context) (*pb.HostInfo, error)
	// HostInfoRefresh is how often host info is re-gathered (default 10m).
	HostInfoRefresh time.Duration
	// Backoff for failed sends (default 5s doubling up to 5m, jittered).
	Backoff *backoff.Backoff
	// DrainPause between replayed spool batches (default 500ms).
	DrainPause time.Duration
	// ShutdownTimeout bounds the final flush (default 5s).
	ShutdownTimeout time.Duration
	// FirstShipDelay overrides the randomised delay before the first batch.
	FirstShipDelay time.Duration
	// IntervalUnit is the duration of one "second" of server config
	// (default time.Second; tests use milliseconds).
	IntervalUnit time.Duration
}

// Runner is one running agent.
type Runner struct {
	opts     Options
	log      *slog.Logger
	instance string
	seq      atomic.Uint64
	acc      *metrics.Accumulator

	mu           sync.Mutex
	remote       config.Remote
	shipEvery    time.Duration
	hostInfo     *pb.HostInfo
	hostInfoHash string
	sendHostInfo bool
	retryAt      time.Time
	cfgChanged   chan struct{}

	lastLatencyMs  atomic.Int64
	sendFailures   atomic.Uint64
	droppedBatches atomic.Uint64
	shipped        atomic.Uint64
}

// New returns a runner.
func New(opts Options) *Runner {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Collectors == nil {
		opts.Collectors = collector.System()
	}
	if opts.HostInfo == nil {
		opts.HostInfo = collector.HostInfo
	}
	if opts.HostInfoRefresh == 0 {
		opts.HostInfoRefresh = 10 * time.Minute
	}
	if opts.Backoff == nil {
		opts.Backoff = backoff.New(5*time.Second, 5*time.Minute)
	}
	if opts.DrainPause == 0 {
		opts.DrainPause = 500 * time.Millisecond
	}
	if opts.IntervalUnit == 0 {
		opts.IntervalUnit = time.Second
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	r := &Runner{
		opts:       opts,
		log:        opts.Logger,
		instance:   config.NewInstanceID(),
		acc:        metrics.NewAccumulator(maxWindowSamples),
		cfgChanged: make(chan struct{}, 1),
	}
	r.applyRemote(opts.State.Remote.Normalize(), 0, false)
	return r
}

// Run collects and ships until ctx is cancelled, then flushes.
func (r *Runner) Run(ctx context.Context) error {
	r.refreshHostInfo(ctx)
	r.log.Info("agent started", "version", version.Version, "instance", r.instance,
		"api", r.opts.Client.BaseURL, "spooled_batches", r.opts.Spool.Len())

	collectCtx, stopCollect := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { r.collectLoop(collectCtx) })

	r.shipLoop(ctx)

	stopCollect()
	wg.Wait()

	flushCtx, cancel := context.WithTimeout(context.Background(), r.opts.ShutdownTimeout)
	defer cancel()
	r.ship(flushCtx, true)
	r.log.Info("agent stopped", "spooled_batches", r.opts.Spool.Len())
	return nil
}

// Remote returns the active server-driven config.
func (r *Runner) Remote() config.Remote {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.remote
}

func (r *Runner) collectLoop(ctx context.Context) {
	// Prime counter-based collectors (CPU, network) so the first aligned
	// round already has deltas.
	r.collectRound(ctx, time.Now())

	for {
		interval := time.Duration(r.Remote().CollectionInterval) * r.opts.IntervalUnit
		now := time.Now()
		next := now.Truncate(interval).Add(interval)
		timer := time.NewTimer(next.Sub(now))

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.cfgChanged:
			timer.Stop()
			continue
		case <-timer.C:
			r.collectRound(ctx, next)
		}
	}
}

func (r *Runner) collectRound(ctx context.Context, at time.Time) {
	remote := r.Remote()
	ts := at.UnixMilli()
	timeout := max(time.Duration(remote.CollectionInterval)*r.opts.IntervalUnit/2, time.Second)

	for _, c := range r.opts.Collectors {
		if !remote.Enabled(c.Name()) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		err := c.Collect(cctx, at, func(metric string, labels []metrics.Label, value float64) {
			r.acc.Add(metric, labels, ts, value)
		})
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			r.log.Debug("collector failed", "collector", c.Name(), "err", err)
		}
	}
}

func (r *Runner) shipLoop(ctx context.Context) {
	first := r.opts.FirstShipDelay
	if first == 0 {
		// Spread a fleet's batches over the interval instead of all at :00.
		first = time.Duration(rand.Int64N(int64(r.shipInterval())))
	}
	shipTimer := time.NewTimer(first)
	defer shipTimer.Stop()
	retryTimer := time.NewTimer(time.Hour)
	retryTimer.Stop()
	defer retryTimer.Stop()
	hostInfoTicker := time.NewTicker(r.opts.HostInfoRefresh)
	defer hostInfoTicker.Stop()

	// Replay anything left over from a previous run.
	if r.opts.Spool.Len() > 0 {
		r.drain(ctx)
	}

	for {
		r.armRetry(retryTimer)
		select {
		case <-ctx.Done():
			return
		case <-shipTimer.C:
			r.ship(ctx, false)
			shipTimer.Reset(r.shipInterval())
		case <-retryTimer.C:
			r.drain(ctx)
		case <-hostInfoTicker.C:
			r.refreshHostInfo(ctx)
		}
	}
}

func (r *Runner) armRetry(t *time.Timer) {
	r.mu.Lock()
	at := r.retryAt
	r.mu.Unlock()
	t.Stop()
	if !at.IsZero() && r.opts.Spool.Len() > 0 {
		t.Reset(max(time.Until(at), 0))
	}
}

func (r *Runner) shipInterval() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shipEvery
}

func (r *Runner) backingOff() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.retryAt.IsZero() && time.Now().Before(r.retryAt)
}

// ship cuts the current window and sends it, spooling on failure. On the
// final flush nothing is replayed: the spool is kept for the next start.
func (r *Runner) ship(ctx context.Context, final bool) {
	body, err := r.cut()
	if err != nil {
		r.log.Error("encode batch", "err", err)
		return
	}

	if r.opts.Spool.Len() == 0 && !r.backingOff() {
		err := r.send(ctx, body)
		if err == nil {
			return
		}
		if transport.Classify(err) == transport.Drop {
			r.droppedBatches.Add(1)
			r.log.Error("batch rejected by server, dropping", "err", err)
			return
		}
		r.failed(err)
	}

	if err := r.opts.Spool.Push(body, time.Now()); err != nil {
		r.droppedBatches.Add(1)
		r.log.Error("spool batch", "err", err)
		return
	}
	if !final && !r.backingOff() {
		r.drain(ctx)
	}
}

// drain replays spooled batches oldest first until the spool is empty or a
// send fails.
func (r *Runner) drain(ctx context.Context) {
	for ctx.Err() == nil {
		entry, body, ok, err := r.opts.Spool.Oldest()
		if err != nil {
			r.log.Error("read spool", "err", err)
			return
		}
		if !ok {
			return
		}
		err = r.send(ctx, body)
		switch {
		case err == nil:
		case transport.Classify(err) == transport.Drop:
			r.droppedBatches.Add(1)
			r.log.Error("spooled batch rejected by server, dropping", "batch", entry.Name, "err", err)
		default:
			r.failed(err)
			return
		}
		if err := r.opts.Spool.Remove(entry); err != nil {
			r.log.Error("remove spooled batch", "err", err)
			return
		}
		if r.opts.Spool.Len() > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.opts.DrainPause):
			}
		}
	}
}

func (r *Runner) send(ctx context.Context, body []byte) error {
	start := time.Now()
	resp, err := r.opts.Client.Ingest(ctx, body)
	if err != nil {
		r.sendFailures.Add(1)
		return err
	}
	r.lastLatencyMs.Store(time.Since(start).Milliseconds())
	r.shipped.Add(1)

	r.mu.Lock()
	r.retryAt = time.Time{}
	r.opts.Backoff.Reset()
	if resp.ResendHostInfo {
		r.sendHostInfo = true
	}
	r.mu.Unlock()

	r.applyRemote(resp.Config, resp.NextIngestIn, true)
	return nil
}

func (r *Runner) failed(err error) {
	r.mu.Lock()
	delay := r.opts.Backoff.Next()
	if ra := transport.RetryAfter(err); ra > delay {
		delay = ra
	}
	r.retryAt = time.Now().Add(delay)
	attempts := r.opts.Backoff.Attempts()
	r.mu.Unlock()

	var apiErr *transport.Error
	level := slog.LevelWarn
	if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
		level = slog.LevelError // token revoked or host deleted: data spools until fixed
	}
	r.log.Log(context.Background(), level, "ship failed, spooling", "err", err, "retry_in", delay.Round(time.Second), "attempt", attempts)
}

// cut drains the window, appends agent health samples and encodes the batch.
func (r *Runner) cut() ([]byte, error) {
	now := time.Now()
	if r.Remote().Enabled("agent") {
		r.healthSamples(now)
	}
	series := r.acc.Drain()

	r.mu.Lock()
	env := encode.Envelope{
		AgentVersion:  version.Version,
		AgentInstance: r.instance,
		BatchSeq:      r.seq.Add(1),
		CreatedAtMs:   now.UnixMilli(),
		HostInfoHash:  r.hostInfoHash,
	}
	if r.sendHostInfo && r.hostInfo != nil {
		env.HostInfo = r.hostInfo
		// Once queued it is delivered in order; if it is lost (spool
		// eviction) the server asks again via resend_host_info.
		r.sendHostInfo = false
	}
	r.mu.Unlock()

	return encode.Marshal(encode.Build(env, series))
}

func (r *Runner) healthSamples(now time.Time) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	ts := now.UnixMilli()
	add := func(metric string, v float64) { r.acc.Add(metric, nil, ts, v) }

	add("agent.spool.bytes", float64(r.opts.Spool.Bytes()))
	add("agent.spool.batches", float64(r.opts.Spool.Len()))
	add("agent.batches.dropped", float64(r.droppedBatches.Load()+r.opts.Spool.Dropped()))
	add("agent.samples.dropped", float64(r.acc.Dropped()))
	add("agent.send.failures", float64(r.sendFailures.Load()))
	add("agent.send.latency_ms", float64(r.lastLatencyMs.Load()))
	add("agent.memory.heap_mb", float64(int64(ms.HeapAlloc/1024))/1024)
	add("agent.memory.sys_mb", float64(int64(ms.Sys/1024))/1024)
}

func (r *Runner) refreshHostInfo(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := r.opts.HostInfo(cctx)
	if err != nil {
		r.log.Warn("gather host info", "err", err)
		return
	}
	hash := encode.HostInfoHash(info)

	r.mu.Lock()
	defer r.mu.Unlock()
	if hash != r.hostInfoHash {
		r.hostInfo = info
		r.hostInfoHash = hash
		r.sendHostInfo = true
	}
}

// applyRemote installs a server config; nextIngestIn (seconds) overrides the
// shipping interval when positive.
func (r *Runner) applyRemote(remote config.Remote, nextIngestIn int, persist bool) {
	remote = remote.Normalize()

	r.mu.Lock()
	changed := !remote.Equal(r.remote)
	collectionChanged := remote.CollectionInterval != r.remote.CollectionInterval
	r.remote = remote
	every := remote.IngestInterval
	if nextIngestIn > 0 {
		every = min(max(nextIngestIn, 10), 600)
	}
	r.shipEvery = time.Duration(every) * r.opts.IntervalUnit
	r.mu.Unlock()

	if !changed {
		return
	}
	for _, c := range r.opts.Collectors {
		if cc, ok := c.(collector.Configurable); ok {
			cc.Configure(remote)
		}
	}
	if collectionChanged {
		select {
		case r.cfgChanged <- struct{}{}:
		default:
		}
	}
	if persist {
		r.log.Info("applied server config", "collection_interval", remote.CollectionInterval,
			"ingest_interval", remote.IngestInterval, "collectors", remote.Collectors)
		if r.opts.StateDir != "" {
			st := r.opts.State
			st.Remote = remote
			if err := config.SaveState(r.opts.StateDir, st); err != nil {
				r.log.Warn("persist server config", "err", err)
			}
		}
	}
}
