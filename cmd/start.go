package cmd

import (
	"context"
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
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		agent := service.NewApp()

		agent.Start(ctx)
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
}
