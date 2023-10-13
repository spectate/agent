//go:build darwin

package logger

import (
	"log"
	"os"
)

var (
	BaseDir  string
	AgentLog string
)

func init() {
	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get user home directory: %v", err)
	}
	BaseDir = userHomeDir + "/.spectated/logs"
	AgentLog = BaseDir + "/spectated.log"
}

func initLoggerConstants() {}
