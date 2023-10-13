//go:build !darwin

package logger

const (
	BaseDir  = "/var/log/spectate-agent"
	AgentLog = "/var/log/spectate-agent/spectated.log"
)

func initLoggerConstants() {}
