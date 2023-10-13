package cmd

import (
	logger "github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/internal/version"
	"github.com/spectate/agent/pkg/config"
	"github.com/spectate/agent/pkg/telemetry"
	"os"

	"github.com/spf13/cobra"
)

var verbose bool

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "spectated",
	Short: "Spectated is a lightweight monitoring agent",
	Long: `Spectated is Spectate's lightweight monitoring agent that can be used to monitor a server
by running it in the background.

Version: ` + version.Version + `
Built on: ` + version.BuildDate,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(
		config.InitConfig,
		func() {
			logger.InitLogger(verbose)
		},
		telemetry.InitTelemetry,
	)
	defer logger.ShutdownLogger()

	rootCmd.PersistentFlags().StringVar(&config.CfgFile, "config", "", "config file (default is "+config.CfgFile+")")

	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")
}
