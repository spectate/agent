package strategies

import (
	"context"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/internal/shared"
	"github.com/spectate/agent/pkg/metrics"
	"github.com/spectate/agent/pkg/proto/pb"
	"time"
)

type Memory struct {
	opts Options
}

func init() {
	Registrar.RegisterStrategy(&Memory{
		opts: Options{
			Namespace: "system",
			Id:        "memory",
			Frequency: time.Minute,
		},
	})
}

func (s *Memory) Options() Options {
	return s.opts
}

func (s *Memory) Run(ctx context.Context, dataCh chan<- []*pb.MetricsPayload_Metric) error {
	batch := metrics.NewMetricsBatch(dataCh)

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Running strategy")

	vMem, err := mem.VirtualMemory()
	if err != nil {
		return err
	}

	// Values are returned in bytes, convert to MB

	batch.Gauge("system.memory.total", float64(vMem.Total)/shared.MbSize, nil)
	batch.Gauge("system.memory.free", float64(vMem.Free)/shared.MbSize, nil)
	batch.Gauge("system.memory.used", float64(vMem.Used)/shared.MbSize, nil)
	batch.Gauge("system.memory.available", float64(vMem.Available)/shared.MbSize, nil)
	batch.Gauge("system.memory.available_percent", 100-vMem.UsedPercent, nil)

	swap, err := mem.SwapMemory()
	if err != nil {
		return err
	}

	batch.Gauge("system.swap.total", float64(swap.Total)/shared.MbSize, nil)
	batch.Gauge("system.swap.free", float64(swap.Free)/shared.MbSize, nil)
	batch.Gauge("system.swap.used", float64(swap.Used)/shared.MbSize, nil)
	batch.Gauge("system.swap.in", float64(swap.Sin)/shared.MbSize, nil)
	batch.Gauge("system.swap.out", float64(swap.Sout)/shared.MbSize, nil)
	batch.Gauge("system.swap.free_percent", 100-swap.UsedPercent, nil)

	batch.Commit()

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Completed strategy")

	return nil
}
