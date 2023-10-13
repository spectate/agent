package strategies

import (
	"context"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/pkg/metrics"
	"github.com/spectate/agent/pkg/proto/pb"
	"time"
)

type LoadAvg struct {
	opts Options
}

func init() {
	Registrar.RegisterStrategy(&LoadAvg{
		opts: Options{
			Namespace: "system",
			Id:        "load",
			Frequency: time.Minute,
		},
	})
}

func (s *LoadAvg) Options() Options {
	return s.opts
}

func (s *LoadAvg) Run(ctx context.Context, dataCh chan<- []*pb.MetricsPayload_Metric) error {
	batch := metrics.NewMetricsBatch(dataCh)

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Running strategy")

	loadAvg, err := load.Avg()
	if err != nil {
		return err
	}

	batch.Gauge("system.load.1", loadAvg.Load1, nil)
	batch.Gauge("system.load.5", loadAvg.Load5, nil)
	batch.Gauge("system.load.15", loadAvg.Load15, nil)

	batch.Commit()

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Completed strategy")

	return nil
}
