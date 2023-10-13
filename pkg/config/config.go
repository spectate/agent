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
	} else {
		initDefaults()
		logger.Log.Debug().
			Msg("No config file found, writing defaults to file")
		err := viper.WriteConfigAs(BaseDir + "/config.yaml")
		if err != nil {
			logger.Log.Panic().Err(err).Msg("Failed to write config file")
			os.Exit(1)
		}
	}

}

func initDefaults() {
	viper.SetDefault("version", 1)
	viper.SetDefault("host.token", "")
	viper.SetDefault("telemetry.error_reporting", true)
}

func Update() {
	err := viper.WriteConfigAs(BaseDir + "/config.yaml")
	if err != nil {
		logger.Log.Panic().Err(err).Msg("Failed to write config file")
		os.Exit(1)
	}
}
