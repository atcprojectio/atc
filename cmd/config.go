package cmd

import (
	"fmt"

	"github.com/spf13/viper"

	"github.com/atcprojectio/atc/pkg/atc"
)

// loadCLIConfig reads the --config file (if any) and resolves settings with the same
// precedence as "atc server": command-line flags, then ATC_* environment variables, then
// the YAML file.
func loadCLIConfig() (atc.Config, error) {
	if configFile := viper.GetString("config"); configFile != "" {
		viper.SetConfigFile(configFile)
		if err := viper.ReadInConfig(); err != nil {
			return atc.Config{}, fmt.Errorf("failed to read config file %s: %w", configFile, err)
		}
	}
	return atc.LoadConfigFromViper(viper.GetViper())
}
