package logger

import (
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"
	"github.com/spectate/agent/internal/version"
	"gopkg.in/natefinch/lumberjack.v2"
	"io"
	"os"
	"time"
)

var (
	Log      zerolog.Logger
	file     *os.File
	LogLevel = "debug"
)

func InitLogger(verbose bool) {
	initLoggerConstants()

	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	//goland:noinspection GoBoolExpressions We set Environment in the build process
	isDevelopment := version.Environment == "development"

	var output io.Writer = zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	}

	level, err := zerolog.ParseLevel(LogLevel)
	if err != nil {
		panic(err)
	}

	if !isDevelopment {
		fileLogger := &lumberjack.Logger{
			Filename:   AgentLog,
			MaxSize:    10,
			MaxBackups: 5,
			MaxAge:     7,
			Compress:   true,
		}

		if verbose {
			output = zerolog.MultiLevelWriter(os.Stdout, fileLogger)
		} else {
			output = zerolog.MultiLevelWriter(fileLogger)
		}
		zerolog.SetGlobalLevel(level)
	}

	if verbose {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	Log = zerolog.New(output).
		With().
		Timestamp().
		Str("version", version.Version).
		Logger()
}

func ShutdownLogger() {
	if file != nil {
		file.Close()
	}
}
