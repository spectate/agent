package config

import (
	"fmt"
	"github.com/spectate/agent/internal/logger"
	"github.com/spf13/viper"
	"os"
)

var CfgFile string

// InitConfig reads in config file and ENV variables if set.
func InitConfig() {
	initConfigConstants()

	if CfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(CfgFile)
	} else {
		// Check if the config directory exists, if not, create it.
		if _, err := os.Stat(BaseDir); os.IsNotExist(err) {
			err = os.MkdirAll(BaseDir, 0755)
			if err != nil {
				fmt.Println("Failed to create config directory:", err)
				os.Exit(1)
			}
		}

		// Search config in directory with name "config" (without extension).
		viper.AddConfigPath(BaseDir)
		viper.SetConfigType("yaml")
		viper.SetConfigName("config")
	}

	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in. Else write defaults to file.
	if err := viper.ReadInConfig(); err == nil {
		logger.Log.Debug().
			Msg("Using config file: " + viper.ConfigFileUsed())
	}

	setDefaults()

	err := viper.WriteConfigAs(BaseDir + "/config.yaml")
	if err != nil {
		logger.Log.Panic().Err(err).Msg("Failed to write config file")
		os.Exit(1)
	}

}

func setDefaults() {
	viper.SetDefault("telemetry.error_reporting", true)

	viper.SetDefault("checks.disk_usage.exclude.devices", []string{})
	viper.SetDefault("checks.disk_usage.exclude.filesystems", []string{"squashfs", "vfat"})
	viper.SetDefault("checks.disk_usage.exclude.mount_points", []string{})

	// We have excluded some interfaces by default to avoid collecting metrics from
	// interfaces that are not relevant to the user in most setups. For example, we don't want to
	// collect metrics from docker interfaces.

	// Include is used to override wildcards in exclude. Include does not support wildcards.
	viper.SetDefault("checks.net.include.interfaces", []string{"en0", "eth0", "lo0"})
	viper.SetDefault("checks.net.exclude.interfaces", []string{"br*", "docker*", "lo", "veth*", "utun*", "llw*", "utun*", "bridge*", "ap*", "anpi*", "awdl*", "gif*", "stf*", "en*"})

	viper.SetDefault("host.token", "")
	viper.SetDefault("version", 1)
}

func Update() {
	err := viper.WriteConfigAs(BaseDir + "/config.yaml")
	if err != nil {
		logger.Log.Panic().Err(err).Msg("Failed to write config file")
		os.Exit(1)
	}
}
