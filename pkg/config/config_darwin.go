//go:build darwin

package config

import (
	"log"
	"os"
)

var (
	BaseDir string
)

func init() {
	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get user home directory: %v", err)
	}
	BaseDir = userHomeDir + "/.spectated"
}

func initConfigConstants() {}
