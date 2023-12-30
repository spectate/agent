package strategies

import (
	"context"
	"github.com/shirou/gopsutil/v3/net"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/internal/shared"
	"github.com/spectate/agent/pkg/metrics"
	"github.com/spectate/agent/pkg/proto/pb"
	"github.com/spf13/viper"
	"strings"
	"time"
)

type Net struct {
	opts               Options
	includedInterfaces []string
	excludedInterfaces []string
	lastIOCounters     []net.IOCountersStat
}

func init() {
	Registrar.RegisterStrategy(&Net{
		opts: Options{
			Namespace: "system",
			Id:        "net",
			Frequency: time.Minute,
		},
	})
}

func (s *Net) Options() Options {
	return s.opts
}

func (s *Net) Run(ctx context.Context, dataCh chan<- []*pb.MetricsPayload_Metric) error {
	batch := metrics.NewMetricsBatch(dataCh)

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Running strategy")

	netIOCounters, err := net.IOCounters(true)
	if err != nil {
		return err
	}

	s.includedInterfaces = viper.GetStringSlice("checks.net.include.interfaces")
	s.excludedInterfaces = viper.GetStringSlice("checks.net.exclude.interfaces")

	// We call a network interface "iface" to avoid confusion with the Go interface type
	for _, ioCount := range netIOCounters {
		if s.isExcluded(ioCount.Name) {
			continue
		}

		var lastIOCount *net.IOCountersStat = nil

		for i, l := range s.lastIOCounters {
			if l.Name == ioCount.Name {
				lastIOCount = &l

				// Update the lastIOCounters list with the new value
				s.lastIOCounters[i] = ioCount
				break
			}
		}

		// If we don't have a lastIOCount, add it to the list and continue to the next
		// interface as we can't calculate the difference
		if lastIOCount == nil {
			s.lastIOCounters = append(s.lastIOCounters, ioCount)
			continue
		}

		labels := make([]*pb.MetricsPayload_Label, 0)
		labels = append(labels, &pb.MetricsPayload_Label{
			Key:   "interface",
			Value: ioCount.Name,
		})

		// Convert to KB and use the difference between the current and last counter value
		batch.Gauge("system.net.bytes_sent", float64(ioCount.BytesSent-lastIOCount.BytesSent)/1024, labels)
		batch.Gauge("system.net.bytes_recv", float64(ioCount.BytesRecv-lastIOCount.BytesRecv)/1024, labels)
		batch.Gauge("system.net.packets_sent", float64(ioCount.PacketsSent-lastIOCount.PacketsSent), labels)
		batch.Gauge("system.net.packets_recv", float64(ioCount.PacketsRecv-lastIOCount.PacketsRecv), labels)
		batch.Gauge("system.net.err_in", float64(ioCount.Errin-lastIOCount.Errin), labels)
		batch.Gauge("system.net.err_out", float64(ioCount.Errout-lastIOCount.Errout), labels)
		batch.Gauge("system.net.drop_in", float64(ioCount.Dropin-lastIOCount.Dropin), labels)
		batch.Gauge("system.net.drop_out", float64(ioCount.Dropout-lastIOCount.Dropout), labels)
		batch.Gauge("system.net.fifo_in", float64(ioCount.Fifoin-lastIOCount.Fifoin), labels)
		batch.Gauge("system.net.fifo_out", float64(ioCount.Fifoout-lastIOCount.Fifoout), labels)
	}

	batch.Commit()

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Completed strategy")

	return nil
}

func (s *Net) isExcluded(iface string) bool {
	if shared.StrSliceContains(s.includedInterfaces, iface) {
		// Included interfaces take precedence over excluded interfaces
		return false
	}

	for _, excludedInterface := range s.excludedInterfaces {
		// If the interface is an exact match or a wildcard match, return true
		if iface == excludedInterface ||
			(strings.HasSuffix(excludedInterface, "*") && strings.HasPrefix(iface, excludedInterface[:len(excludedInterface)-1])) {
			return true
		}
	}

	return false
}
