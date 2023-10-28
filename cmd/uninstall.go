package cmd

import (
	"fmt"
	"github.com/spectate/agent/pkg/service"
	"github.com/spf13/cobra"
)

// uninstallCmd represents the start command
var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall the spectated service",
	Long:  `Uninstalls spectated from the system's service manager.`,
	Run: func(cmd *cobra.Command, args []string) {
		s, err := service.NewService()
		if err != nil {
			panic(err)
		}
		if err := s.Uninstall(); err != nil {
			fmt.Printf("Failed to install service: %s\n", err)
		} else {
			fmt.Println("Spectated has been uninstalled.")
		}
	},
}

func init() {
	rootCmd.AddCommand(uninstallCmd)
}
