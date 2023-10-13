package strategies

import (
	"context"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/pkg/metrics"
	"github.com/spectate/agent/pkg/proto/pb"
	"time"
)

type DiskUsage struct {
	opts Options
}

func init() {
	Registrar.RegisterStrategy(&DiskUsage{
		opts: Options{
			Namespace: "system",
			Id:        "disk_usage",
			Frequency: time.Minute,
		},
	})
}

func (s *DiskUsage) Options() Options {
	return s.opts
}

func (s *DiskUsage) Run(ctx context.Context, dataCh chan<- []*pb.MetricsPayload_Metric) error {
	batch := metrics.NewMetricsBatch(dataCh)

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Running strategy")

	partitionInfo, err := disk.Partitions(false)
	if err != nil {
		return err
	}

	for _, partition := range partitionInfo {
		usage, err := disk.Usage(partition.Mountpoint)
		if err != nil {
			logger.Log.Error().Err(err).Msgf("Failed to get disk usage for %s", partition.Mountpoint)
			continue
		}

		labels := make([]*pb.MetricsPayload_Label, 0)
		labels = append(labels, &pb.MetricsPayload_Label{
			Key:   "mountpoint",
			Value: partition.Mountpoint,
		})
		labels = append(labels, &pb.MetricsPayload_Label{
			Key:   "device",
			Value: partition.Device,
		})
		labels = append(labels, &pb.MetricsPayload_Label{
			Key:   "fstype",
			Value: partition.Fstype,
		})

		// Convert to KB and percent
		batch.Gauge("system.disk_usage.total", float64(usage.Total)/1024, labels)
		batch.Gauge("system.disk_usage.free", float64(usage.Free)/1024, labels)
		batch.Gauge("system.disk_usage.used", float64(usage.Used)/1024, labels)
		batch.Gauge("system.disk_usage.used_percent", usage.UsedPercent/100, labels)
		batch.Gauge("system.disk_usage.inodes_total", float64(usage.InodesTotal), labels)
		batch.Gauge("system.disk_usage.inodes_used", float64(usage.InodesUsed), labels)
		batch.Gauge("system.disk_usage.inodes_free", float64(usage.InodesFree), labels)
		batch.Gauge("system.disk_usage.inodes_used_percent", usage.InodesUsedPercent/100, labels)
	}

	batch.Commit()

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Completed strategy")

	return nil
}
