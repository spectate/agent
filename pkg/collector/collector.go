package collector

import (
	"context"
	"github.com/getsentry/sentry-go"
	"github.com/spectate/agent/internal/http"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/pkg/proto/pb"
	"time"
)

type Collector struct {
	client        *http.Client
	collectedData []*pb.MetricsPayload_Metric
	dataCh        <-chan []*pb.MetricsPayload_Metric
}

func NewCollector(client *http.Client, dataCh <-chan []*pb.MetricsPayload_Metric) *Collector {
	logger.Log.Info().Msg("Initializing collector")
	return &Collector{
		client:        client,
		collectedData: []*pb.MetricsPayload_Metric{},
		dataCh:        dataCh,
	}
}

func (c *Collector) CollectAndSend(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Minute)

	logger.Log.Debug().Msg("Starting collector")

	for {
		select {
		case <-ticker.C:
			// Send collected data every minute
			if len(c.collectedData) > 0 {
				err := c.sendData(c.collectedData)
				if err != nil {
					logger.Log.Error().Err(err).Msg("Failed to send data to API. Collecting data queued next cycle.")
					sentry.CaptureException(err)
				} else {
					// Clear data after sending
					c.collectedData = []*pb.MetricsPayload_Metric{}
				}
			}
		case data := <-c.dataCh:
			c.collectedData = append(c.collectedData, data...)
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *Collector) sendData(data []*pb.MetricsPayload_Metric) error {
	logger.Log.Debug().Msg("Pushing collected data to API")

	_, err := c.client.Payload(&pb.MetricsPayload{
		Metrics: data,
	})

	if err != nil {
		logger.Log.Error().Err(err).Msg("Failed to send data to API")
		return err
	}

	return nil
}
