package strategies

import (
	"context"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/internal/shared"
	"github.com/spectate/agent/pkg/metrics"
	"github.com/spectate/agent/pkg/proto/pb"
	"github.com/spf13/viper"
	"strings"
	"time"
)

type DiskUsage struct {
	opts                Options
	excludedDevices     []string
	excludedFilesystems []string
	excludedMountPoints []string
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

	s.excludedFilesystems = viper.GetStringSlice("checks.disk_usage.exclude.filesystems")
	s.excludedDevices = viper.GetStringSlice("checks.disk_usage.exclude.devices")
	s.excludedMountPoints = viper.GetStringSlice("checks.disk_usage.exclude.mount_points")

	for _, partition := range partitionInfo {
		if s.isExcluded(partition) {
			continue
		}

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
		batch.Gauge("system.disk_usage.used_percent", usage.UsedPercent, labels)
		batch.Gauge("system.disk_usage.inodes_total", float64(usage.InodesTotal), labels)
		batch.Gauge("system.disk_usage.inodes_used", float64(usage.InodesUsed), labels)
		batch.Gauge("system.disk_usage.inodes_free", float64(usage.InodesFree), labels)
		batch.Gauge("system.disk_usage.inodes_used_percent", usage.InodesUsedPercent, labels)
	}

	batch.Commit()

	logger.Log.Debug().
		Str("namespace", s.opts.Namespace).
		Str("id", s.opts.Id).
		Msg("Completed strategy")

	return nil
}

func (s *DiskUsage) isExcluded(partition disk.PartitionStat) bool {
	if shared.StrSliceContains(s.excludedFilesystems, strings.ToLower(partition.Fstype)) {
		return true
	}

	if shared.StrSliceContains(s.excludedDevices, strings.ToLower(partition.Device)) {
		return true
	}

	if shared.StrSliceContains(s.excludedMountPoints, strings.ToLower(partition.Mountpoint)) {
		return true
	}

	return false
}
