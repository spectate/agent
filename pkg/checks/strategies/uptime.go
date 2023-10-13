package strategies

import (
	"context"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/pkg/metrics"
	"github.com/spectate/agent/pkg/proto/pb"
	"time"
)

type Uptime struct {
	opts Options
}

func init() {
	Registrar.RegisterStrategy(&Uptime{
		opts: Options{
			Namespace: "system",
			Id:        "uptime",
			Frequency: time.Minute,
		},
	})
}

func (s *Uptime) Options() Options {
	return s.opts
}

func (s *Uptime) Run(ctx context.Context, dataCh chan<- []*pb.MetricsPayload_Metric) error {
	batch := metrics.NewMetricsBatch(dataCh)

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Running strategy")

	uptime, err := host.Uptime()
	if err != nil {
		return err
	}

	batch.Gauge("system.uptime", float64(uptime), nil)

	batch.Commit()

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Completed strategy")

	return nil
}
