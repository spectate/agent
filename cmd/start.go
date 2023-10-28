package cmd

import (
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/pkg/service"
	"github.com/spf13/cobra"
)

// startCmd represents the start command
var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start Spectated",
	Long: `Start the Spectated agent. This will start the agent in the background and will
	monitor the server.`,
	Run: func(cmd *cobra.Command, args []string) {
		s, err := service.NewService()
		if err != nil {
			logger.Log.Panic().Err(err).Msg("Failed to create service")
		}

		if err := s.Run(); err != nil {
			logger.Log.Panic().Err(err).Msg("Failed to run service")
		}
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
}
