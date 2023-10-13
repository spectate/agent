package strategies

import (
	"context"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/pkg/metrics"
	"github.com/spectate/agent/pkg/proto/pb"
	"time"
)

type CpuTimes struct {
	opts      Options
	cores     float64
	lastCycle float64
	lastTimes cpu.TimesStat
}

func init() {
	cpuInfo, err := cpu.Info()
	if err != nil {
		logger.Log.Error().Err(err).Msg("Failed to get CPU info")
		return
	}
	var cores float64
	for _, info := range cpuInfo {
		cores += float64(info.Cores)
	}
	Registrar.RegisterStrategy(&CpuTimes{
		opts: Options{
			Namespace: "system",
			Id:        "cpu",
			Frequency: time.Minute,
		},
		cores: cores,
	})
}

func (s *CpuTimes) Options() Options {
	return s.opts
}

func (s *CpuTimes) Run(ctx context.Context, dataCh chan<- []*pb.MetricsPayload_Metric) error {
	batch := metrics.NewMetricsBatch(dataCh)

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Running strategy")

	cpuTimes, err := cpu.Times(false)
	if err != nil {
		logger.Log.Error().Err(err).Msg("Failed to get CPU times")
		return err
	} else if len(cpuTimes) < 1 {
		logger.Log.Error().Msg("No CPU times found")
		return nil
	}

	t := cpuTimes[0]

	total := t.User + t.System + t.Idle + t.Nice +
		t.Iowait + t.Irq + t.Softirq + t.Steal
	cycle := total / s.cores

	if s.lastCycle != 0 {
		toPercent := 100 / (cycle - s.lastCycle)

		user := ((t.User + t.Nice) - (s.lastTimes.User + s.lastTimes.Nice)) / s.cores
		system := ((t.System + t.Irq + t.Softirq) - (s.lastTimes.System + s.lastTimes.Irq + s.lastTimes.Softirq)) / s.cores
		interrupt := ((t.Irq + t.Softirq) - (s.lastTimes.Irq + s.lastTimes.Softirq)) / s.cores
		iowait := (t.Iowait - s.lastTimes.Iowait) / s.cores
		idle := (t.Idle - s.lastTimes.Idle) / s.cores
		stolen := (t.Steal - s.lastTimes.Steal) / s.cores
		guest := (t.Guest - s.lastTimes.Guest) / s.cores

		batch.Gauge("system.cpu.user", user*toPercent, nil)
		batch.Gauge("system.cpu.system", system*toPercent, nil)
		batch.Gauge("system.cpu.interrupt", interrupt*toPercent, nil)
		batch.Gauge("system.cpu.iowait", iowait*toPercent, nil)
		batch.Gauge("system.cpu.idle", idle*toPercent, nil)
		batch.Gauge("system.cpu.stolen", stolen*toPercent, nil)
		batch.Gauge("system.cpu.guest", guest*toPercent, nil)
	}

	batch.Commit()

	s.lastCycle = cycle
	s.lastTimes = t

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Completed strategy")

	return nil
}
