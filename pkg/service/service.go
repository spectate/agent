package service

import (
	"context"
	"fmt"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/spectate/agent/internal/http"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/internal/version"
	"github.com/spectate/agent/pkg/checks/strategies"
	"github.com/spectate/agent/pkg/collector"
	"github.com/spectate/agent/pkg/metrics"
	"github.com/spectate/agent/pkg/proto/pb"
	"github.com/spf13/viper"
	"strconv"
	"strings"
	"sync"
	"time"
)

type App struct {
	client     *http.Client
	strategies map[time.Duration][]strategies.Strategy
	dataCh     chan []*pb.MetricsPayload_Metric
	wg         sync.WaitGroup
}

func NewApp() *App {
	logger.Log.Info().Msg("Starting Spectated " + version.Version + " (Build date: " + version.BuildDate + ")")

	client := http.NewClient()

	registeredStrategies := strategies.Registrar.Strategies

	return &App{
		client:     client,
		strategies: registeredStrategies,
	}
}

func (app *App) Start(ctx context.Context) {
	tokenValue := viper.Get("host.token")
	token, ok := tokenValue.(string)
	if !ok || token == "" {
		fmt.Println("No token found. Please follow the instructions on https://app.spectate.net to set up your host.")
		return
	}

	app.dataCh = make(chan []*pb.MetricsPayload_Metric)

	go func() {
		err := app.initialMetrics(ctx)
		if err != nil {
			logger.Log.Err(err).Msg("Failed to send initial metrics")
			return
		}
	}()

	app.bootstrapMonitoring(ctx)

	coll := collector.NewCollector(app.client, app.dataCh)
	err := coll.CollectAndSend(ctx)
	if err != nil {
		logger.Log.Err(err)
		return
	}
}

func (app *App) Stop() {
	logger.Log.Info().Msg("Stopping Spectated")
}

// initialMetrics sends metrics that are not collected periodically, such
// as the host info, cpu info, agent version, etc.
func (app *App) initialMetrics(ctx context.Context) error {
	logger.Log.Debug().Msg("Collect initial metrics")

	batch := metrics.NewMetricsBatch(app.dataCh)
	batch.Info("agent.version", []*pb.MetricsPayload_Label{
		{
			Key:   "version",
			Value: version.Version,
		},
	}, nil)

	hostInfo, err := host.Info()
	if err != nil {
		logger.Log.Err(err).Msg("Failed to get host info")
		return err
	}

	batch.Info("host.info", []*pb.MetricsPayload_Label{
		{
			Key:   "hostname",
			Value: hostInfo.Hostname,
		},
		{
			Key:   "os",
			Value: hostInfo.OS,
		},
		{
			Key:   "platform",
			Value: hostInfo.Platform,
		},
		{
			Key:   "platform_family",
			Value: hostInfo.PlatformFamily,
		},
		{
			Key:   "platform_version",
			Value: hostInfo.PlatformVersion,
		},
		{
			Key:   "kernel_version",
			Value: hostInfo.KernelVersion,
		},
		{
			Key:   "kernel_arch",
			Value: hostInfo.KernelArch,
		},
		{
			Key:   "virtualization_system",
			Value: hostInfo.VirtualizationSystem,
		},
		{
			Key:   "virtualization_role",
			Value: hostInfo.VirtualizationRole,
		},
		{
			Key:   "host_id",
			Value: hostInfo.HostID,
		},
	}, nil)

	cpuInfo, err := cpu.Info()
	if err != nil {
		logger.Log.Err(err).Msg("Failed to get cpu info")
		return err
	}

	for _, c := range cpuInfo {
		batch.Info("host.cpus", []*pb.MetricsPayload_Label{
			{
				Key:   "vendor_id",
				Value: c.VendorID,
			},
			{
				Key:   "family",
				Value: c.Family,
			},
			{
				Key:   "model",
				Value: c.Model,
			},
			{
				Key:   "stepping",
				Value: strconv.Itoa(int(c.Stepping)),
			},
			{
				Key:   "physical_id",
				Value: c.PhysicalID,
			},
			{
				Key:   "core_id",
				Value: c.CoreID,
			},
			{
				Key:   "cores",
				Value: strconv.Itoa(int(c.Cores)),
			},
			{
				Key:   "model_name",
				Value: c.ModelName,
			},
			{
				Key:   "mhz",
				Value: strconv.Itoa(int(c.Mhz)),
			},
			{
				Key:   "cache_size",
				Value: strconv.Itoa(int(c.CacheSize)),
			},
			{
				Key:   "flags",
				Value: strings.Join(c.Flags, ","),
			},
			{
				Key:   "microcode",
				Value: c.Microcode,
			},
		},
			[]*pb.MetricsPayload_Label{
				{
					Key:   "cpu",
					Value: strconv.Itoa(int(c.CPU)),
				},
			},
		)
	}

	batch.Commit()

	logger.Log.Debug().Msg("Committed initial metrics")

	return nil
}

func (app *App) bootstrapMonitoring(ctx context.Context) {
	// For all registered strategies, log them
	for period, strategySlice := range app.strategies {
		for _, strategy := range strategySlice {
			logger.Log.Info().Str("strategy", strategy.Options().Namespace+"."+strategy.Options().Id).Str("frequency", period.String()).Msg("Registered strategy")
		}
	}

	for period, strategySlice := range app.strategies {
		for _, strategy := range strategySlice {
			strategy := strategy

			// Run all strategies once
			go func(s strategies.Strategy) {
				err := s.Run(ctx, app.dataCh)
				if err != nil {
					// TODO: Handle errors appropriately. Skip this strategy for now
					logger.Log.Err(err)
					return
				}
			}(strategy)

			// Start ticker
			go func(s strategies.Strategy, period time.Duration) {
				logger.Log.Debug().Msg("Running " + s.Options().Id + " every " + period.String())
				ticker := time.NewTicker(period)
				defer ticker.Stop()

				for {
					select {
					case <-ticker.C:
						err := s.Run(ctx, app.dataCh)
						if err != nil {
							// TODO: Handle errors appropriately. Skip this strategy for now
							logger.Log.Err(err)
							continue
						}
					case <-ctx.Done():
						return
					}
				}
			}(strategy, period)
		}
	}
}
