package cmd

import (
	"fmt"
	"github.com/spectate/agent/pkg/auth"
	"github.com/spf13/cobra"
)

// startCmd represents the start command
var authCmd = &cobra.Command{
	Use:     "auth",
	Short:   "Authorize this server with Spectate",
	Long:    `Authorize this server with your Spectate team using the provided token.`,
	Example: "spectated auth <token>",
	Args:    cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		err := auth.Authorize(args[0])
		if err != nil {
			fmt.Println("Authentication has failed, please try again. If the problem persists, please contact us.")
		}
	},
}

func init() {
	rootCmd.AddCommand(authCmd)
}
