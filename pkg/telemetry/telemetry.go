package telemetry

import (
	"github.com/getsentry/sentry-go"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/internal/version"
	"github.com/spf13/viper"
)

var SentryDsn string

func InitTelemetry() {
	enabled := viper.GetBool("telemetry.error_reporting")

	if !enabled {
		logger.Log.Info().Msg("Telemetry disabled, skipping Sentry initialization")
		return
	}

	if SentryDsn == "" {
		logger.Log.Warn().Msg("SENTRY_DSN not set, skipping Sentry initialization")
		return
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:              SentryDsn,
		TracesSampleRate: 1.0,
		Release:          version.Version,
	})

	if err != nil {
		logger.Log.Fatal().Msgf("sentry.Init: %s", err)
	}

	return
}
