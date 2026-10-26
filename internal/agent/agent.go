// Package agent wires collection, batching, spooling and shipping.
//
//	collect loop ── every collection_interval (aligned to the wall clock) ──► Accumulator
//	ship loop    ── every next_ingest_in ──► encode + gzip ──► send ──(fail)──► spool
//	                                                      └── spool non-empty ──► replay oldest first
//
// Failed sends back off exponentially with jitter (honouring Retry-After);
// batches the server rejects as malformed or too large are dropped. A
// rejected host token (401) keeps spooling but retries only hourly; a deleted
// host (410 HOST_DELETED) stops the agent for good (see ErrHostDeleted). On
// shutdown the current window is flushed (or spooled when offline).
//
// Collectors run concurrently, each in its own goroutine, bounded by the
// round deadline: a collector stuck in a syscall (statfs on a dead NFS mount)
// delays nothing else, and is not started again until it returns.
package agent

import (
	"context"
	"errors"
	"fmt"
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

// HostDeletedMessage is logged when the API reports the host as deleted.
const HostDeletedMessage = "This host was removed from Spectate. Remove the agent: sudo spectate-agent uninstall"

// ErrHostDeleted is returned by Run when the host was deleted in Spectate
// (now, or by a previous run: state.json carries the host_deleted marker).
// The process should exit without being restarted.
var ErrHostDeleted = errors.New("host deleted in Spectate")

const (
	// clockWarnThreshold is the clock offset beyond which the agent warns.
	clockWarnThreshold = 30 * time.Second
	clockWarnEvery     = time.Hour
	authErrorLogEvery  = time.Hour
	updateLogEvery     = 24 * time.Hour
)

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
	// AuthRetry is the retry interval while the API rejects the host token
	// (401), jittered down by up to a quarter (default 1h).
	AuthRetry time.Duration
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

	cancel  context.CancelFunc
	deleted atomic.Bool
	limits  *limiter

	stateMu sync.Mutex
	state   config.State // last persisted state

	collMu    sync.Mutex
	running   map[string]time.Time // collector name -> start of a run that has not returned
	reportedS map[string]bool      // running collectors already reported as stuck
	lastRun   map[string]time.Time // periodic collector name -> round it last ran in

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
		opts.Collectors = collector.System(opts.StateDir)
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
	if opts.AuthRetry == 0 {
		opts.AuthRetry = time.Hour
	}
	r := &Runner{
		opts:       opts,
		log:        opts.Logger,
		instance:   config.NewInstanceID(),
		acc:        metrics.NewAccumulator(maxWindowSamples),
		cfgChanged: make(chan struct{}, 1),
		limits:     newLimiter(),
		state:      opts.State,
		running:    make(map[string]time.Time),
		reportedS:  make(map[string]bool),
		lastRun:    make(map[string]time.Time),
	}
	r.applyRemote(opts.State.Remote.Normalize(), 0, false)
	return r
}

// Run collects and ships until ctx is cancelled, then flushes. It returns
// ErrHostDeleted, without collecting or shipping anything more, once the API
// reports the host as deleted.
func (r *Runner) Run(ctx context.Context) error {
	if r.opts.State.HostDeleted {
		r.log.Error(HostDeletedMessage, "host_uuid", r.opts.State.HostUUID)
		return ErrHostDeleted
	}
	ctx, r.cancel = context.WithCancel(ctx)
	defer r.cancel()

	r.refreshHostInfo(ctx)
	r.log.Info("agent started", "version", version.Version, "instance", r.instance,
		"api", r.opts.Client.BaseURL, "spooled_batches", r.opts.Spool.Len())

	collectCtx, stopCollect := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { r.collectLoop(collectCtx) })

	r.shipLoop(ctx)

	stopCollect()
	wg.Wait()

	if r.deleted.Load() {
		return ErrHostDeleted
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), r.opts.ShutdownTimeout)
	defer cancel()
	r.ship(flushCtx, true)
	if r.deleted.Load() {
		return ErrHostDeleted
	}
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

// collectRound runs every enabled collector concurrently and waits for them
// until the round deadline (half the collection interval, at least 1s).
// Samples of collectors that have not returned by then are kept up to the
// deadline; later ones are discarded. A collector whose previous run is still
// going is not started again.
func (r *Runner) collectRound(ctx context.Context, at time.Time) {
	remote := r.Remote()
	ts := at.UnixMilli()
	timeout := max(time.Duration(remote.CollectionInterval)*r.opts.IntervalUnit/2, time.Second)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bufs := make([]*roundBuffer, len(r.opts.Collectors))
	done := make(chan struct{}, len(r.opts.Collectors))
	started := 0
	for i, c := range r.opts.Collectors {
		name := c.Name()
		if !remote.Enabled(name) || !r.due(c, at, remote) || !r.startCollector(name) {
			continue
		}
		buf := &roundBuffer{}
		bufs[i] = buf
		started++
		go func() {
			defer func() { done <- struct{}{} }()
			defer r.finishCollector(name)
			if err := c.Collect(rctx, at, buf.emit); err != nil && !errors.Is(err, context.Canceled) {
				r.log.Debug("collector failed", "collector", name, "err", err)
			}
		}()
	}

wait:
	for range started {
		select {
		case <-done:
		case <-rctx.Done():
			break wait
		}
	}

	for _, buf := range bufs {
		if buf == nil {
			continue
		}
		for _, s := range buf.close() {
			r.acc.Add(s.metric, s.labels, ts, s.value)
		}
	}
	r.reportStuck(timeout)
}

// due reports whether a periodic collector should run in the round at `at`:
// once its interval has passed since its last round (minus half a round, so
// it stays aligned with the rounds). Other collectors run every round.
func (r *Runner) due(c collector.Collector, at time.Time, remote config.Remote) bool {
	p, ok := c.(collector.Periodic)
	if !ok {
		return true
	}
	every := time.Duration(p.IntervalSeconds())*r.opts.IntervalUnit -
		time.Duration(remote.CollectionInterval)*r.opts.IntervalUnit/2
	r.collMu.Lock()
	defer r.collMu.Unlock()
	if last, ran := r.lastRun[c.Name()]; ran && at.Sub(last) < every {
		return false
	}
	r.lastRun[c.Name()] = at
	return true
}

// startCollector claims a collector for this round; false while a previous
// run has not returned.
func (r *Runner) startCollector(name string) bool {
	r.collMu.Lock()
	defer r.collMu.Unlock()
	if _, busy := r.running[name]; busy {
		r.log.Debug("collector still running from an earlier round, skipping", "collector", name)
		return false
	}
	r.running[name] = time.Now()
	return true
}

func (r *Runner) finishCollector(name string) {
	r.collMu.Lock()
	defer r.collMu.Unlock()
	if r.reportedS[name] {
		r.log.Info("stuck collector returned, collecting it again", "collector", name,
			"after", time.Since(r.running[name]).Round(time.Millisecond))
		delete(r.reportedS, name)
	}
	delete(r.running, name)
}

// reportStuck logs, once per stuck run, collectors that missed the deadline.
func (r *Runner) reportStuck(timeout time.Duration) {
	r.collMu.Lock()
	defer r.collMu.Unlock()
	for name := range r.running {
		if !r.reportedS[name] {
			r.reportedS[name] = true
			r.log.Warn("collector did not finish within the round deadline; shipping the other collectors and skipping it until it returns",
				"collector", name, "deadline", timeout)
		}
	}
}

// Stuck returns the collectors whose last run has not returned.
func (r *Runner) Stuck() []string {
	r.collMu.Lock()
	defer r.collMu.Unlock()
	var out []string
	for _, c := range r.opts.Collectors {
		if r.reportedS[c.Name()] {
			out = append(out, c.Name())
		}
	}
	return out
}

type sample struct {
	metric string
	labels []metrics.Label
	value  float64
}

// roundBuffer holds one collector's samples for one round. Emits after
// close (a collector returning after the round deadline) are discarded.
type roundBuffer struct {
	mu      sync.Mutex
	closed  bool
	samples []sample
}

func (b *roundBuffer) emit(metric string, labels []metrics.Label, value float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.samples = append(b.samples, sample{metric, labels, value})
	}
}

func (b *roundBuffer) close() []sample {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	out := b.samples
	b.samples = nil
	return out
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

	if r.deleted.Load() {
		return
	}
	if r.opts.Spool.Len() == 0 && !r.backingOff() {
		err := r.send(ctx, body)
		if err == nil {
			return
		}
		switch transport.Classify(err) {
		case transport.Drop:
			r.droppedBatches.Add(1)
			r.log.Error("batch rejected by server, dropping", "err", err)
			return
		case transport.HostDeleted:
			r.hostDeleted(err)
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
		case transport.Classify(err) == transport.HostDeleted:
			r.hostDeleted(err)
			return
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
	r.checkClock(resp.ClockOffsetMs)
	r.noteUpdate(resp.AgentUpdate)
	return nil
}

func (r *Runner) failed(err error) {
	unauthorized := transport.Classify(err) == transport.Unauthorized

	r.mu.Lock()
	var delay time.Duration
	if unauthorized {
		// Only a human can fix a revoked token: keep spooling, but do not
		// knock on the API every few minutes.
		delay = r.opts.AuthRetry - time.Duration(rand.Int64N(int64(r.opts.AuthRetry/4)+1))
	} else {
		delay = r.opts.Backoff.Next()
	}
	if ra := transport.RetryAfter(err); ra > delay {
		delay = ra
	}
	r.retryAt = time.Now().Add(delay)
	attempts := r.opts.Backoff.Attempts()
	r.mu.Unlock()

	if unauthorized {
		level := slog.LevelDebug
		if r.limits.allow("auth", authErrorLogEvery) {
			level = slog.LevelError
		}
		r.log.Log(context.Background(), level,
			"the API rejected the host token (revoked or invalid); spooling and retrying hourly. Re-register the host if it should keep reporting",
			"err", err, "retry_in", delay.Round(time.Second), "spooled_batches", r.opts.Spool.Len())
		return
	}

	var apiErr *transport.Error
	level := slog.LevelWarn
	if errors.As(err, &apiErr) && apiErr.Status == 403 {
		level = slog.LevelError // forbidden: data spools until fixed
	}
	r.log.Log(context.Background(), level, "ship failed, spooling", "err", err, "retry_in", delay.Round(time.Second), "attempt", attempts)
}

// hostDeleted handles 410 HOST_DELETED: the server will never accept data
// for this host again. Drop the spool and the current window, persist the
// marker so a restart does not resume, log once and stop the runner.
func (r *Runner) hostDeleted(err error) {
	if !r.deleted.CompareAndSwap(false, true) {
		return
	}
	r.log.Error(HostDeletedMessage, "err", err, "host_uuid", r.opts.State.HostUUID)
	if cleared, cerr := r.opts.Spool.Clear(); cerr != nil {
		r.log.Warn("clear spool", "err", cerr)
	} else if cleared > 0 {
		r.log.Debug("cleared spool", "batches", cleared)
	}
	r.acc.Drain()
	r.persist(func(st *config.State) { st.HostDeleted = true })
	if r.cancel != nil {
		r.cancel()
	}
}

// checkClock warns (hourly) when the server reports the host clock off by
// more than 30s. The server corrects sample timestamps itself; the agent
// never shifts its own.
func (r *Runner) checkClock(offsetMs int64) {
	offset := time.Duration(offsetMs) * time.Millisecond
	if offset.Abs() <= clockWarnThreshold || !r.limits.allow("clock", clockWarnEvery) {
		return
	}
	direction := "behind"
	if offset < 0 {
		direction = "ahead"
	}
	r.log.Warn(fmt.Sprintf("host clock is off by %ds; samples are corrected by the server, but fix NTP", int64(offset.Abs().Seconds())),
		"clock_offset_ms", offsetMs, "host_clock", direction)
}

// noteUpdate records a newer release announced by the API (shown by
// `spectate-agent status`) and logs it once a day. The agent cannot replace
// itself (it runs unprivileged on a read-only filesystem).
func (r *Runner) noteUpdate(u *transport.AgentUpdate) {
	var avail *config.AvailableUpdate
	if u != nil && version.Compare(u.LatestVersion, version.Version) > 0 {
		avail = &config.AvailableUpdate{Version: u.LatestVersion, ReleaseURL: u.ReleaseURL}
	}

	r.stateMu.Lock()
	prev := r.state.AvailableUpdate
	r.stateMu.Unlock()
	if (prev == nil) != (avail == nil) || (prev != nil && *prev != *avail) {
		r.persist(func(st *config.State) { st.AvailableUpdate = avail })
	}
	if avail != nil && r.limits.allow("update:"+avail.Version, updateLogEvery) {
		r.log.Info(fmt.Sprintf("spectate-agent %s is available: sudo spectate-agent update", avail.Version),
			"current", version.Version, "release_url", avail.ReleaseURL)
	}
}

// persist applies fn to the persisted state and saves it.
func (r *Runner) persist(fn func(*config.State)) {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	fn(&r.state)
	if r.opts.StateDir == "" {
		return
	}
	if err := config.SaveState(r.opts.StateDir, r.state); err != nil {
		r.log.Warn("persist state", "err", err)
	}
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
		r.persist(func(st *config.State) { st.Remote = remote })
	}
}

// limiter rate-limits repeated log lines per key.
type limiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

func newLimiter() *limiter { return &limiter{last: make(map[string]time.Time), now: time.Now} }

// allow reports whether key was not allowed within the last `every`.
func (l *limiter) allow(key string, every time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if last, ok := l.last[key]; ok && now.Sub(last) < every {
		return false
	}
	l.last[key] = now
	return true
}
