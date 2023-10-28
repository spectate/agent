package cmd

import (
	"fmt"
	"github.com/spectate/agent/pkg/service"
	"github.com/spf13/cobra"
)

// installCmd represents the start command
var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install spectated as service",
	Long:  `Installs spectated as a service into the system's service manager.`,
	Run: func(cmd *cobra.Command, args []string) {
		s, err := service.NewService()
		if err != nil {
			panic(err)
		}
		if err := s.Install(); err != nil {
			fmt.Printf("Failed to install service: %s\n", err)
		} else {
			fmt.Println("Spectated has been installed successfully.")
		}
	},
}

func init() {
	rootCmd.AddCommand(installCmd)
}
